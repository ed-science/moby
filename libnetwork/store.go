package libnetwork

import (
	"fmt"
	"strings"

	"github.com/docker/docker/libnetwork/datastore"
	"github.com/docker/libkv/store/boltdb"
	"github.com/sirupsen/logrus"
)

func registerKVStores() {
	boltdb.Register()
}

func (c *Controller) initScopedStore(scope string, scfg *datastore.ScopeCfg) error {
	store, err := datastore.NewDataStore(scope, scfg)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.stores = append(c.stores, store)
	c.mu.Unlock()

	return nil
}

func (c *Controller) initStores() error {
	registerKVStores()

	c.mu.Lock()
	if c.cfg == nil {
		c.mu.Unlock()
		return nil
	}
	scopeConfigs := c.cfg.Scopes
	c.stores = nil
	c.mu.Unlock()

	for scope, scfg := range scopeConfigs {
		if err := c.initScopedStore(scope, scfg); err != nil {
			return err
		}
	}

	c.startWatch()
	return nil
}

func (c *Controller) closeStores() {
	for _, store := range c.getStores() {
		store.Close()
	}
}

func (c *Controller) getStore(scope string) datastore.DataStore {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, store := range c.stores {
		if store.Scope() == scope {
			return store
		}
	}

	return nil
}

func (c *Controller) getStores() []datastore.DataStore {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.stores
}

func (c *Controller) getNetworkFromStore(nid string) (*network, error) {
	for _, n := range c.getNetworksFromStore() {
		if n.id == nid {
			return n, nil
		}
	}
	return nil, ErrNoSuchNetwork(nid)
}

func (c *Controller) getNetworksForScope(scope string) ([]*network, error) {
	var nl []*network

	store := c.getStore(scope)
	if store == nil {
		return nil, nil
	}

	kvol, err := store.List(datastore.Key(datastore.NetworkKeyPrefix),
		&network{ctrlr: c})
	if err != nil && err != datastore.ErrKeyNotFound {
		return nil, fmt.Errorf("failed to get networks for scope %s: %v",
			scope, err)
	}

	for _, kvo := range kvol {
		n := kvo.(*network)
		n.ctrlr = c

		ec := &endpointCnt{n: n}
		err = store.GetObject(datastore.Key(ec.Key()...), ec)
		if err != nil && !n.inDelete {
			logrus.Warnf("Could not find endpoint count key %s for network %s while listing: %v", datastore.Key(ec.Key()...), n.Name(), err)
			continue
		}

		n.epCnt = ec
		if n.scope == "" {
			n.scope = scope
		}
		nl = append(nl, n)
	}

	return nl, nil
}

func (c *Controller) getNetworksFromStore() []*network {
	var nl []*network

	for _, store := range c.getStores() {
		kvol, err := store.List(datastore.Key(datastore.NetworkKeyPrefix), &network{ctrlr: c})
		// Continue searching in the next store if no keys found in this store
		if err != nil {
			if err != datastore.ErrKeyNotFound {
				logrus.Debugf("failed to get networks for scope %s: %v", store.Scope(), err)
			}
			continue
		}

		kvep, err := store.Map(datastore.Key(epCntKeyPrefix), &endpointCnt{})
		if err != nil && err != datastore.ErrKeyNotFound {
			logrus.Warnf("failed to get endpoint_count map for scope %s: %v", store.Scope(), err)
		}

		for _, kvo := range kvol {
			n := kvo.(*network)
			n.mu.Lock()
			n.ctrlr = c
			ec := &endpointCnt{n: n}
			// Trim the leading & trailing "/" to make it consistent across all stores
			if val, ok := kvep[strings.Trim(datastore.Key(ec.Key()...), "/")]; ok {
				ec = val.(*endpointCnt)
				ec.n = n
				n.epCnt = ec
			}
			if n.scope == "" {
				n.scope = store.Scope()
			}
			n.mu.Unlock()
			nl = append(nl, n)
		}
	}

	return nl
}

func (n *network) getEndpointFromStore(eid string) (*Endpoint, error) {
	var errors []string
	for _, store := range n.ctrlr.getStores() {
		ep := &Endpoint{id: eid, network: n}
		err := store.GetObject(datastore.Key(ep.Key()...), ep)
		// Continue searching in the next store if the key is not found in this store
		if err != nil {
			if err != datastore.ErrKeyNotFound {
				errors = append(errors, fmt.Sprintf("{%s:%v}, ", store.Scope(), err))
				logrus.Debugf("could not find endpoint %s in %s: %v", eid, store.Scope(), err)
			}
			continue
		}
		return ep, nil
	}
	return nil, fmt.Errorf("could not find endpoint %s: %v", eid, errors)
}

func (n *network) getEndpointsFromStore() ([]*Endpoint, error) {
	var epl []*Endpoint

	tmp := Endpoint{network: n}
	for _, store := range n.getController().getStores() {
		kvol, err := store.List(datastore.Key(tmp.KeyPrefix()...), &Endpoint{network: n})
		// Continue searching in the next store if no keys found in this store
		if err != nil {
			if err != datastore.ErrKeyNotFound {
				logrus.Debugf("failed to get endpoints for network %s scope %s: %v",
					n.Name(), store.Scope(), err)
			}
			continue
		}

		for _, kvo := range kvol {
			ep := kvo.(*Endpoint)
			epl = append(epl, ep)
		}
	}

	return epl, nil
}

func (c *Controller) updateToStore(kvObject datastore.KVObject) error {
	cs := c.getStore(kvObject.DataScope())
	if cs == nil {
		return ErrDataStoreNotInitialized(kvObject.DataScope())
	}

	if err := cs.PutObjectAtomic(kvObject); err != nil {
		if err == datastore.ErrKeyModified {
			return err
		}
		return fmt.Errorf("failed to update store for object type %T: %v", kvObject, err)
	}

	return nil
}

func (c *Controller) deleteFromStore(kvObject datastore.KVObject) error {
	cs := c.getStore(kvObject.DataScope())
	if cs == nil {
		return ErrDataStoreNotInitialized(kvObject.DataScope())
	}

retry:
	if err := cs.DeleteObjectAtomic(kvObject); err != nil {
		if err == datastore.ErrKeyModified {
			if err := cs.GetObject(datastore.Key(kvObject.Key()...), kvObject); err != nil {
				return fmt.Errorf("could not update the kvobject to latest when trying to delete: %v", err)
			}
			logrus.Warnf("Error (%v) deleting object %v, retrying....", err, kvObject.Key())
			goto retry
		}
		return err
	}

	return nil
}

type netWatch struct {
	localEps  map[string]*Endpoint
	remoteEps map[string]*Endpoint
	stopCh    chan struct{}
}

func (c *Controller) getLocalEps(nw *netWatch) []*Endpoint {
	c.mu.Lock()
	defer c.mu.Unlock()

	var epl []*Endpoint
	for _, ep := range nw.localEps {
		epl = append(epl, ep)
	}

	return epl
}

func (c *Controller) watchSvcRecord(ep *Endpoint) {
	c.watchCh <- ep
}

func (c *Controller) unWatchSvcRecord(ep *Endpoint) {
	c.unWatchCh <- ep
}

func (c *Controller) networkWatchLoop(nw *netWatch, ep *Endpoint, ecCh <-chan datastore.KVObject) {
	for {
		select {
		case <-nw.stopCh:
			return
		case o := <-ecCh:
			ec := o.(*endpointCnt)

			epl, err := ec.n.getEndpointsFromStore()
			if err != nil {
				break
			}

			c.mu.Lock()
			var addEp []*Endpoint

			delEpMap := make(map[string]*Endpoint)
			renameEpMap := make(map[string]bool)
			for k, v := range nw.remoteEps {
				delEpMap[k] = v
			}

			for _, lEp := range epl {
				if _, ok := nw.localEps[lEp.ID()]; ok {
					continue
				}

				if ep, ok := nw.remoteEps[lEp.ID()]; ok {
					// On a container rename EP ID will remain
					// the same but the name will change. service
					// records should reflect the change.
					// Keep old EP entry in the delEpMap and add
					// EP from the store (which has the new name)
					// into the new list
					if lEp.name == ep.name {
						delete(delEpMap, lEp.ID())
						continue
					}
					renameEpMap[lEp.ID()] = true
				}
				nw.remoteEps[lEp.ID()] = lEp
				addEp = append(addEp, lEp)
			}

			// EPs whose name are to be deleted from the svc records
			// should also be removed from nw's remote EP list, except
			// the ones that are getting renamed.
			for _, lEp := range delEpMap {
				if !renameEpMap[lEp.ID()] {
					delete(nw.remoteEps, lEp.ID())
				}
			}
			c.mu.Unlock()

			for _, lEp := range delEpMap {
				ep.getNetwork().updateSvcRecord(lEp, c.getLocalEps(nw), false)
			}
			for _, lEp := range addEp {
				ep.getNetwork().updateSvcRecord(lEp, c.getLocalEps(nw), true)
			}
		}
	}
}

func (c *Controller) processEndpointCreate(nmap map[string]*netWatch, ep *Endpoint) {
	n := ep.getNetwork()
	if !c.isDistributedControl() && n.Scope() == datastore.SwarmScope && n.driverIsMultihost() {
		return
	}

	networkID := n.ID()
	endpointID := ep.ID()

	c.mu.Lock()
	nw, ok := nmap[networkID]
	c.mu.Unlock()

	if ok {
		// Update the svc db for the local endpoint join right away
		n.updateSvcRecord(ep, c.getLocalEps(nw), true)

		c.mu.Lock()
		nw.localEps[endpointID] = ep

		// If we had learned that from the kv store remove it
		// from remote ep list now that we know that this is
		// indeed a local endpoint
		delete(nw.remoteEps, endpointID)
		c.mu.Unlock()
		return
	}

	nw = &netWatch{
		localEps:  make(map[string]*Endpoint),
		remoteEps: make(map[string]*Endpoint),
	}

	// Update the svc db for the local endpoint join right away
	// Do this before adding this ep to localEps so that we don't
	// try to update this ep's container's svc records
	n.updateSvcRecord(ep, c.getLocalEps(nw), true)

	c.mu.Lock()
	nw.localEps[endpointID] = ep
	nmap[networkID] = nw
	nw.stopCh = make(chan struct{})
	c.mu.Unlock()

	store := c.getStore(n.DataScope())
	if store == nil {
		return
	}

	if !store.Watchable() {
		return
	}

	ch, err := store.Watch(n.getEpCnt(), nw.stopCh)
	if err != nil {
		logrus.Warnf("Error creating watch for network: %v", err)
		return
	}

	go c.networkWatchLoop(nw, ep, ch)
}

func (c *Controller) processEndpointDelete(nmap map[string]*netWatch, ep *Endpoint) {
	n := ep.getNetwork()
	if !c.isDistributedControl() && n.Scope() == datastore.SwarmScope && n.driverIsMultihost() {
		return
	}

	networkID := n.ID()
	endpointID := ep.ID()

	c.mu.Lock()
	nw, ok := nmap[networkID]

	if ok {
		delete(nw.localEps, endpointID)
		c.mu.Unlock()

		// Update the svc db about local endpoint leave right away
		// Do this after we remove this ep from localEps so that we
		// don't try to remove this svc record from this ep's container.
		n.updateSvcRecord(ep, c.getLocalEps(nw), false)

		c.mu.Lock()
		if len(nw.localEps) == 0 {
			close(nw.stopCh)

			// This is the last container going away for the network. Destroy
			// this network's svc db entry
			delete(c.svcRecords, networkID)

			delete(nmap, networkID)
		}
	}
	c.mu.Unlock()
}

func (c *Controller) watchLoop() {
	for {
		select {
		case ep := <-c.watchCh:
			c.processEndpointCreate(c.nmap, ep)
		case ep := <-c.unWatchCh:
			c.processEndpointDelete(c.nmap, ep)
		}
	}
}

func (c *Controller) startWatch() {
	if c.watchCh != nil {
		return
	}
	c.watchCh = make(chan *Endpoint)
	c.unWatchCh = make(chan *Endpoint)
	c.nmap = make(map[string]*netWatch)

	go c.watchLoop()
}

func (c *Controller) networkCleanup() {
	for _, n := range c.getNetworksFromStore() {
		if n.inDelete {
			logrus.Infof("Removing stale network %s (%s)", n.Name(), n.ID())
			if err := n.delete(true, true); err != nil {
				logrus.Debugf("Error while removing stale network: %v", err)
			}
		}
	}
}

var populateSpecial NetworkWalker = func(nw Network) bool {
	if n := nw.(*network); n.hasSpecialDriver() && !n.ConfigOnly() {
		if err := n.getController().addNetwork(n); err != nil {
			logrus.Warnf("Failed to populate network %q with driver %q", nw.Name(), nw.Type())
		}
	}
	return false
}
