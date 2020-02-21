// Copyright 2020 Netflix Inc
// Author: Colin McIntosh (colin@netflix.com)
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Copyright 2018 Google Inc.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//     http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Portions of this file including TargetState and its receivers (excluding modifications) are from
// https://github.com/openconfig/gnmi/blob/89b2bf29312cda887da916d0f3a32c1624b7935f/cmd/gnmi_collector/gnmi_collector.go
package connections

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/golang/protobuf/proto"
	"github.com/openconfig/gnmi/cache"
	"github.com/openconfig/gnmi/client"
	gnmiclient "github.com/openconfig/gnmi/client/gnmi"
	gnmipb "github.com/openconfig/gnmi/proto/gnmi"
	targetpb "github.com/openconfig/gnmi/proto/target"
	"golang.org/x/sync/semaphore"
	"stash.corp.netflix.com/ocnas/gnmi-gateway/gateway/configuration"
	"stash.corp.netflix.com/ocnas/gnmi-gateway/gateway/locking"
)

// TargetState makes the calls to connect a target, tracks any associated connection state, and is the container for
// the target's cache data. It is created once for every device and used as a closure parameter by ProtoHandler.
type TargetState struct {
	config      *configuration.GatewayConfig
	name        string
	targetCache *cache.Target
	// connected status is set to true when the first gnmi notification is received.
	// it gets reset to false when disconnect call back of ReconnectClient is called.
	connected bool
	// connecting status is used to signal that some of the connection process has been started and
	// full reconnection is necessary if the target configuration changes
	connecting bool
	client     *client.ReconnectClient
	// stopped status signals that .disconnect() has been called we no longer want to connect to this target so we
	// should stop trying to connect and release any locks that are being held
	stopped bool
	target  *targetpb.Target
	request *gnmipb.SubscribeRequest
}

func (t *TargetState) Equal(other *targetpb.Target) bool {
	if len(t.target.Addresses) != len(other.Addresses) {
		return false
	}
	for i, addr := range t.target.Addresses {
		if other.Addresses[i] != addr {
			return false
		}
	}
	if t.target.Credentials.Username != other.Credentials.Username {
		return false
	}
	if t.target.Credentials.Password != other.Credentials.Password {
		return false
	}
	return true
}

func (t *TargetState) connect() {
	t.connecting = true
	t.config.Log.Info().Msgf("Connecting to target %s", t.name)
	query, err := client.NewQuery(t.request)
	if err != nil {
		t.config.Log.Error().Msgf("NewQuery(%s): %v", t.request.String(), err)
		return
	}
	query.Addrs = t.target.Addresses

	if t.target.Credentials != nil {
		query.Credentials = &client.Credentials{
			Username: t.target.Credentials.Username,
			Password: t.target.Credentials.Password,
		}
	}

	// TLS is always enabled for a targetCache.
	query.TLS = &tls.Config{
		// Today, we assume that we should not verify the certificate from the targetCache.
		InsecureSkipVerify: true,
	}

	query.Target = t.name
	query.Timeout = t.config.TargetDialTimeout

	query.ProtoHandler = t.handleUpdate

	if err := query.Validate(); err != nil {
		t.config.Log.Error().Err(err).Msgf("query.Validate(): %v", err)
		return
	}
	t.client = client.Reconnect(&client.BaseClient{}, t.disconnected, nil)
	// Subscribe blocks until .Close() is called
	if err := t.client.Subscribe(context.Background(), query, gnmiclient.Type); err != nil {
		t.config.Log.Error().Err(err).Msgf("Subscribe failed for targetCache %q: %v", t.name, err)
	}
}

// Attempt to acquire a connection slot. After a connection slot is acquired attempt to grab the lock for the target.
// After the lock for the target is acquired connect to the target. If TargetState.disconnect() is called
// all attempts and connections are aborted.
func (t *TargetState) connectWithLock(connectionSlot *semaphore.Weighted, lock locking.NonBlockingLocker) {
	var connectionSlotAcquired = false
	var connectionLockAcquired = false
	for !t.stopped {
		if !connectionSlotAcquired {
			connectionSlotAcquired = connectionSlot.TryAcquire(1)
		}
		if connectionSlotAcquired {
			if !connectionLockAcquired {
				connectionLockAcquired = lock.Try()
			}
			if connectionLockAcquired {
				t.connect()
			}
		}
	}
	if connectionSlotAcquired {
		connectionSlot.Release(1)
	}
	if connectionLockAcquired {
		lock.Unlock()
	}
}

// Disconnect from the target or stop trying to connect.
func (t *TargetState) disconnect() error {
	t.stopped = true
	return t.client.Close() // this will disconnect and reset the cache via the disconnect callback
}

// Callback for gNMI client to signal that it has disconnected.
func (t *TargetState) disconnected() {
	t.connected = false
	t.targetCache.Disconnect()
	t.targetCache.Reset()
}

func (t *TargetState) reconnect() error {
	return t.client.Close()
}

// handleUpdate parses a protobuf message received from the targetCache. This implementation handles only
// gNMI SubscribeResponse messages. When the message is an Update, the GnmiUpdate method of the
// cache.Target is called to generate an update. If the message is a sync_response, then targetCache is
// marked as synchronised.
func (t *TargetState) handleUpdate(msg proto.Message) error {
	//fmt.Printf("%+v\n", msg)
	if !t.connected {
		t.targetCache.Connect()
		t.connected = true
	}
	resp, ok := msg.(*gnmipb.SubscribeResponse)
	if !ok {
		return fmt.Errorf("failed to type assert message %#v", msg)
	}
	switch v := resp.Response.(type) {
	case *gnmipb.SubscribeResponse_Update:
		// Gracefully handle gNMI implementations that do not set Prefix.Target in their
		// SubscribeResponse Updates.
		if v.Update.GetPrefix() == nil {
			v.Update.Prefix = &gnmipb.Path{}
		}
		if v.Update.Prefix.Target == "" {
			v.Update.Prefix.Target = t.name
		}
		if err := t.rejectUpdate(v.Update); err != nil {
			//t.config.Log.Warn().Msgf("Update rejected: %t: %+v", err, v.Update)
			return nil
		}
		err := t.targetCache.GnmiUpdate(v.Update)
		if err != nil {
			return fmt.Errorf("targetCache cache update error: %t: %+v", err, v.Update)
		}
	case *gnmipb.SubscribeResponse_SyncResponse:
		t.config.Log.Debug().Msgf("Target is synced: %s", t.name)
		t.targetCache.Sync()
	case *gnmipb.SubscribeResponse_Error:
		return fmt.Errorf("error in response: %s", v)
	default:
		return fmt.Errorf("unknown response %T: %t", v, v)
	}
	return nil
}

func (t *TargetState) rejectUpdate(notification *gnmipb.Notification) error {
	for _, update := range notification.GetUpdate() {
		path := update.GetPath().GetElem()
		if len(path) >= 2 {
			if path[0].Name == "interfaces" && path[1].Name == "interface" {
				if value, exists := path[1].Key["name"]; exists {
					if value == "interface" {
						return errors.New("bug for Arista interface path") // Arista BUG #??????????
					}
				}
			}
			if path[0].Name == "network-instances" && path[1].Name == "network-instance" {
				if value, exists := path[1].Key["name"]; exists {
					if value == "network-instance" {
						return errors.New("bug for Arista isis adjacency path") // Arista BUG #??????????
					}
				}
			}
			if path[0].Name == "netconf-state" {
				return errors.New("bug for netconf-state path")
			}
		}
	}
	return nil
}
