// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package ddl

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/clientv3/concurrency"
	"github.com/coreos/etcd/etcdserver/api/v3rpc/rpctypes"
	"github.com/coreos/etcd/mvcc/mvccpb"
	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/tidb/terror"
	goctx "golang.org/x/net/context"
	"google.golang.org/grpc"
)

// OwnerManager is used to campaign the owner and manage the owner information.
type OwnerManager interface {
	// ID returns the ID of the manager.
	ID() string
	// IsOwner returns whether the ownerManager is the owner.
	IsOwner() bool
	// SetOwner sets whether the ownerManager is the owner.
	SetOwner(isOwner bool)
	// GetOwnerID gets the owner ID.
	GetOwnerID(ctx goctx.Context) (string, error)
	// CampaignOwner campaigns the owner.
	CampaignOwner(ctx goctx.Context) error
	// Cancel cancels this etcd ownerManager campaign.
	Cancel()
}

const (
	// DDLOwnerKey is the ddl owner path that is saved to etcd, and it's exported for testing.
	DDLOwnerKey               = "/tidb/ddl/fg/owner"
	ddlPrompt                 = "ddl"
	newSessionDefaultRetryCnt = 3
	newSessionRetryUnlimited  = math.MaxInt64
)

// ownerManager represents the structure which is used for electing owner.
type ownerManager struct {
	owner   int32
	id      string // id is the ID of the manager.
	key     string
	prompt  string
	etcdCli *clientv3.Client
	cancel  goctx.CancelFunc
}

// NewOwnerManager creates a new OwnerManager.
func NewOwnerManager(etcdCli *clientv3.Client, prompt, id, key string, cancel goctx.CancelFunc) OwnerManager {
	return &ownerManager{
		etcdCli: etcdCli,
		id:      id,
		key:     key,
		prompt:  prompt,
		cancel:  cancel,
	}
}

// ID implements OwnerManager.ID interface.
func (m *ownerManager) ID() string {
	return m.id
}

// IsOwner implements OwnerManager.IsOwner interface.
func (m *ownerManager) IsOwner() bool {
	return atomic.LoadInt32(&m.owner) == 1
}

// SetOwner implements OwnerManager.SetOwner interface.
func (m *ownerManager) SetOwner(isOwner bool) {
	if isOwner {
		atomic.StoreInt32(&m.owner, 1)
	} else {
		atomic.StoreInt32(&m.owner, 0)
	}
}

// Cancel implements OwnerManager.Cancel interface.
func (m *ownerManager) Cancel() {
	m.cancel()
}

// ManagerSessionTTL is the etcd session's TTL in seconds. It's exported for testing.
var ManagerSessionTTL = 60

// setManagerSessionTTL sets the ManagerSessionTTL value, it's used for testing.
func setManagerSessionTTL() error {
	ttlStr := os.Getenv("tidb_manager_ttl")
	if len(ttlStr) == 0 {
		return nil
	}
	ttl, err := strconv.Atoi(ttlStr)
	if err != nil {
		return errors.Trace(err)
	}
	ManagerSessionTTL = ttl
	return nil
}

func newSession(ctx goctx.Context, prompt string, flag string, etcdCli *clientv3.Client, retryCnt, ttl int) (*concurrency.Session, error) {
	var err error
	var etcdSession *concurrency.Session
	for i := 0; i < retryCnt; i++ {
		etcdSession, err = concurrency.NewSession(etcdCli,
			concurrency.WithTTL(ttl), concurrency.WithContext(ctx))
		if err == nil {
			break
		}
		log.Warnf("[%s] %s failed to new session, err %v", prompt, flag, err)
		if isContextFinished(err) || terror.ErrorEqual(err, grpc.ErrClientConnClosing) {
			break
		}
		time.Sleep(200 * time.Millisecond)
		continue
	}
	return etcdSession, errors.Trace(err)
}

// CampaignOwner implements OwnerManager.CampaignOwner interface.
func (m *ownerManager) CampaignOwner(ctx goctx.Context) error {
	session, err := newSession(ctx, m.prompt, m.key, m.etcdCli, newSessionDefaultRetryCnt, ManagerSessionTTL)
	if err != nil {
		return errors.Trace(err)
	}
	cancelCtx, _ := goctx.WithCancel(ctx)
	go m.campaignLoop(cancelCtx, session)
	return nil
}

func (m *ownerManager) campaignLoop(ctx goctx.Context, etcdSession *concurrency.Session) {
	idInfo := fmt.Sprintf("%s ownerManager %s", m.key, m.id)
	var err error
	for {
		select {
		case <-etcdSession.Done():
			log.Infof("[%s] %s etcd session is done, creates a new one", m.prompt, idInfo)
			etcdSession, err = newSession(ctx, m.prompt, idInfo, m.etcdCli, newSessionRetryUnlimited, ManagerSessionTTL)
			if err != nil {
				log.Infof("[%s] %s break campaign loop, err %v", m.prompt, idInfo, err)
				return
			}
		case <-ctx.Done():
			// Revoke the session lease.
			// If revoke takes longer than the ttl, lease is expired anyway.
			cancelCtx, cancel := goctx.WithTimeout(goctx.Background(),
				time.Duration(ManagerSessionTTL)*time.Second)
			_, err = m.etcdCli.Revoke(cancelCtx, etcdSession.Lease())
			cancel()
			log.Infof("[%s] %s break campaign loop err %v", m.prompt, idInfo, err)
			return
		default:
		}
		// If the etcd server turns clocks forward，the following case may occur.
		// The etcd server deletes this session's lease ID, but etcd session doesn't find it.
		// In this time if we do the campaign operation, the etcd server will return ErrLeaseNotFound.
		if terror.ErrorEqual(err, rpctypes.ErrLeaseNotFound) {
			if etcdSession != nil {
				err = etcdSession.Close()
				log.Infof("[%s] %s etcd session encounters the error of lease not found, closes it err %s", m.prompt, idInfo, err)
			}
			continue
		}

		elec := concurrency.NewElection(etcdSession, m.key)
		err = elec.Campaign(ctx, m.id)
		if err != nil {
			log.Infof("[%s] %s failed to campaign, err %v", m.prompt, idInfo, err)
			if isContextFinished(err) {
				log.Warnf("[%s] %s campaign loop, err %v", m.prompt, idInfo, err)
				return
			}
			continue
		}

		ownerKey, err := GetOwnerInfo(ctx, elec, m.prompt, m.key, m.id)
		if err != nil {
			continue
		}
		m.SetOwner(true)

		m.watchOwner(ctx, etcdSession, ownerKey)
		m.SetOwner(false)
	}
}

// GetOwnerID implements OwnerManager.GetOwnerID interface.
func (m *ownerManager) GetOwnerID(ctx goctx.Context) (string, error) {
	resp, err := m.etcdCli.Get(ctx, m.key, clientv3.WithFirstCreate()...)
	if err != nil {
		return "", errors.Trace(err)
	}
	if len(resp.Kvs) == 0 {
		return "", concurrency.ErrElectionNoLeader
	}
	return string(resp.Kvs[0].Value), nil
}

// GetOwnerInfo gets the owner information.
func GetOwnerInfo(ctx goctx.Context, elec *concurrency.Election, prompt, key, id string) (string, error) {
	resp, err := elec.Leader(ctx)
	if err != nil {
		// If no leader elected currently, it returns ErrElectionNoLeader.
		log.Infof("[%s] %s ownerManager %s failed to get leader, err %v", prompt, key, id, err)
		return "", errors.Trace(err)
	}
	ownerID := string(resp.Kvs[0].Value)
	log.Infof("[%s] %s ownerManager is %s, owner is %v", prompt, key, id, ownerID)
	if ownerID != id {
		log.Warnf("[%s] %s ownerManager %s isn't the owner", prompt, key, id)
		return "", errors.New("ownerInfoNotMatch")
	}

	return string(resp.Kvs[0].Key), nil
}

func (m *ownerManager) watchOwner(ctx goctx.Context, etcdSession *concurrency.Session, key string) {
	log.Debugf("[%s] ownerManager %s watch owner key %v", m.prompt, m.id, key)
	watchCh := m.etcdCli.Watch(ctx, key)
	for {
		select {
		case resp := <-watchCh:
			if resp.Canceled {
				log.Infof("[%s] ownerManager %s watch owner key %v failed, no owner",
					m.prompt, m.id, key)
				return
			}

			for _, ev := range resp.Events {
				if ev.Type == mvccpb.DELETE {
					log.Infof("[%s] ownerManager %s watch owner key %v failed, owner is deleted", m.prompt, m.id, key)
					return
				}
			}
		case <-etcdSession.Done():
			return
		case <-ctx.Done():
			return
		}
	}
}
