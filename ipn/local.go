// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ipn

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/wireguard-go/wgcfg"
	"tailscale.com/control/controlclient"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/ipn/policy"
	"tailscale.com/portlist"
	"tailscale.com/tailcfg"
	"tailscale.com/types/empty"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/version"
	"tailscale.com/wgengine"
	"tailscale.com/wgengine/filter"
)

// LocalBackend is the scaffolding between the Tailscale cloud control
// plane and the local network stack, wiring up NetworkMap updates
// from the cloud to the local WireGuard engine.
type LocalBackend struct {
	ctx             context.Context    // valid until Close
	ctxCancel       context.CancelFunc // closes ctx
	logf            logger.Logf
	e               wgengine.Engine
	store           StateStore
	serverURL       string // tailcontrol URL
	backendLogID    string
	portpoll        *portlist.Poller // may be nil
	newDecompressor func() (controlclient.Decompressor, error)
	lastFilterPrint time.Time

	// The mutex protects the following elements.
	mu           sync.Mutex
	notify       func(Notify)
	c            *controlclient.Client // TODO: appears to be (inconsistently) guarded by mu
	stateKey     StateKey
	prefs        *Prefs
	state        State
	hiCache      *tailcfg.Hostinfo
	netMapCache  *controlclient.NetworkMap
	engineStatus EngineStatus
	endpoints    []string
	blocked      bool
	authURL      string
	interact     int

	// statusLock must be held before calling statusChanged.Lock() or
	// statusChanged.Broadcast().
	statusLock    sync.Mutex
	statusChanged *sync.Cond
}

// NewLocalBackend returns a new LocalBackend that is ready to run,
// but is not actually running.
func NewLocalBackend(logf logger.Logf, logid string, store StateStore, e wgengine.Engine) (*LocalBackend, error) {
	if e == nil {
		panic("ipn.NewLocalBackend: wgengine must not be nil")
	}

	// Default filter blocks everything, until Start() is called.
	e.SetFilter(filter.NewAllowNone())

	ctx, cancel := context.WithCancel(context.Background())
	portpoll, err := portlist.NewPoller()
	if err != nil {
		logf("skipping portlist: %s", err)
	}

	b := &LocalBackend{
		ctx:          ctx,
		ctxCancel:    cancel,
		logf:         logf,
		e:            e,
		store:        store,
		backendLogID: logid,
		state:        NoState,
		portpoll:     portpoll,
	}
	b.statusChanged = sync.NewCond(&b.statusLock)

	if b.portpoll != nil {
		go b.portpoll.Run(ctx)
		go b.readPoller()
	}

	return b, nil
}

func (b *LocalBackend) Shutdown() {
	b.ctxCancel()
	b.c.Shutdown()
	b.e.Close()
	b.e.Wait()
}

// Status returns the latest status of the Tailscale network from all the various components.
func (b *LocalBackend) Status() *ipnstate.Status {
	sb := new(ipnstate.StatusBuilder)
	b.UpdateStatus(sb)
	return sb.Status()
}

func (b *LocalBackend) UpdateStatus(sb *ipnstate.StatusBuilder) {
	b.e.UpdateStatus(sb)

	b.mu.Lock()
	defer b.mu.Unlock()

	// TODO: hostinfo, and its networkinfo
	// TODO: EngineStatus copy (and deprecate it?)
	if b.netMapCache != nil {
		for id, up := range b.netMapCache.UserProfiles {
			sb.AddUser(id, up)
		}
		for _, p := range b.netMapCache.Peers {
			var lastSeen time.Time
			if p.LastSeen != nil {
				lastSeen = *p.LastSeen
			}
			var tailAddr string
			if len(p.Addresses) > 0 {
				tailAddr = strings.TrimSuffix(p.Addresses[0].String(), "/32")
			}
			sb.AddPeer(key.Public(p.Key), &ipnstate.PeerStatus{
				InNetworkMap: true,
				UserID:       p.User,
				TailAddr:     tailAddr,
				HostName:     p.Hostinfo.Hostname,
				OS:           p.Hostinfo.OS,
				KeepAlive:    p.KeepAlive,
				Created:      p.Created,
				LastSeen:     lastSeen,
			})
		}
	}

}

// SetDecompressor sets a decompression function, which must be a zstd
// reader.
//
// This exists because the iOS/Mac NetworkExtension is very resource
// constrained, and the zstd package is too heavy to fit in the
// constrained RSS limit.
func (b *LocalBackend) SetDecompressor(fn func() (controlclient.Decompressor, error)) {
	b.newDecompressor = fn
}

func (b *LocalBackend) Start(opts Options) error {
	if opts.Prefs == nil && opts.StateKey == "" {
		return errors.New("no state key or prefs provided")
	}

	if opts.Prefs != nil {
		b.logf("Start: %v", opts.Prefs.Pretty())
	} else {
		b.logf("Start")
	}

	hi := controlclient.NewHostinfo()
	hi.BackendLogID = b.backendLogID
	hi.FrontendLogID = opts.FrontendLogID

	b.mu.Lock()

	if b.c != nil {
		// TODO(apenwarr): avoid the need to reinit controlclient.
		// This will trigger a full relogin/reconfigure cycle every
		// time a Handle reconnects to the backend. Ideally, we
		// would send the new Prefs and everything would get back
		// into sync with the minimal changes. But that's not how it
		// is right now, which is a sign that the code is still too
		// complicated.
		b.c.Shutdown()
	}

	if b.hiCache != nil {
		hi.Services = b.hiCache.Services // keep any previous session and netinfo
		hi.NetInfo = b.hiCache.NetInfo
	}
	b.hiCache = hi
	b.state = NoState

	if err := b.loadStateLocked(opts.StateKey, opts.Prefs, opts.LegacyConfigPath); err != nil {
		b.mu.Unlock()
		return fmt.Errorf("loading requested state: %v", err)
	}

	b.serverURL = b.prefs.ControlURL
	hi.RoutableIPs = append(hi.RoutableIPs, b.prefs.AdvertiseRoutes...)
	hi.RequestTags = append(hi.RequestTags, b.prefs.AdvertiseTags...)

	b.notify = opts.Notify
	b.netMapCache = nil
	persist := b.prefs.Persist
	wantDERP := !b.prefs.DisableDERP
	b.mu.Unlock()

	b.e.SetDERPEnabled(wantDERP)
	b.updateFilter(nil)

	var err error
	if persist == nil {
		// let controlclient initialize it
		persist = &controlclient.Persist{}
	}
	cli, err := controlclient.New(controlclient.Options{
		Logf:            logger.WithPrefix(b.logf, "control: "),
		Persist:         *persist,
		ServerURL:       b.serverURL,
		AuthKey:         opts.AuthKey,
		Hostinfo:        hi,
		KeepAlive:       true,
		NewDecompressor: b.newDecompressor,
	})
	if err != nil {
		return err
	}

	b.mu.Lock()
	b.c = cli
	endpoints := b.endpoints
	b.mu.Unlock()

	if endpoints != nil {
		cli.UpdateEndpoints(0, endpoints)
	}

	cli.SetStatusFunc(func(newSt controlclient.Status) {
		if newSt.LoginFinished != nil {
			// Auth completed, unblock the engine
			b.blockEngineUpdates(false)
			b.authReconfig()
			b.send(Notify{LoginFinished: &empty.Message{}})
		}
		if newSt.Persist != nil {
			persist := *newSt.Persist // copy

			b.mu.Lock()
			b.prefs.Persist = &persist
			prefs := b.prefs.Clone()
			stateKey := b.stateKey
			b.mu.Unlock()

			if stateKey != "" {
				if err := b.store.WriteState(stateKey, prefs.ToBytes()); err != nil {
					b.logf("Failed to save new controlclient state: %v", err)
				}
			}
			b.send(Notify{Prefs: prefs})
		}
		if newSt.NetMap != nil {
			b.mu.Lock()
			if b.netMapCache != nil {
				diff := newSt.NetMap.ConciseDiffFrom(b.netMapCache)
				if strings.TrimSpace(diff) == "" {
					b.logf("netmap diff: (none)")
				} else {
					b.logf("netmap diff:\n%v", diff)
				}
			}
			b.netMapCache = newSt.NetMap
			b.mu.Unlock()

			b.send(Notify{NetMap: newSt.NetMap})
			b.updateFilter(newSt.NetMap)
		}
		if newSt.URL != "" {
			b.logf("Received auth URL: %.20v...", newSt.URL)

			b.mu.Lock()
			interact := b.interact
			b.authURL = newSt.URL
			b.mu.Unlock()

			if interact > 0 {
				b.popBrowserAuthNow()
			}
		}
		if newSt.Err != "" {
			// TODO(crawshaw): display in the UI.
			log.Print(newSt.Err)
			return
		}
		if newSt.NetMap != nil {
			b.mu.Lock()
			if b.state == NeedsLogin {
				b.prefs.WantRunning = true
			}
			prefs := b.prefs
			b.mu.Unlock()

			b.SetPrefs(prefs)
		}
		b.stateMachine()
	})

	b.e.SetStatusCallback(func(s *wgengine.Status, err error) {
		if err != nil {
			b.logf("wgengine status error: %#v", err)
			return
		}
		if s == nil {
			log.Fatalf("weird: non-error wgengine update with status=nil")
		}

		es := b.parseWgStatus(s)

		b.mu.Lock()
		c := b.c
		b.engineStatus = es
		b.endpoints = append([]string{}, s.LocalAddrs...)
		b.mu.Unlock()

		if c != nil {
			c.UpdateEndpoints(0, s.LocalAddrs)
		}
		b.stateMachine()

		b.statusLock.Lock()
		b.statusChanged.Broadcast()
		b.statusLock.Unlock()

		b.send(Notify{Engine: &es})
	})

	b.e.SetNetInfoCallback(b.SetNetInfo)

	b.mu.Lock()
	prefs := b.prefs.Clone()
	b.mu.Unlock()

	blid := b.backendLogID
	b.logf("Backend: logs: be:%v fe:%v", blid, opts.FrontendLogID)
	b.send(Notify{BackendLogID: &blid})
	b.send(Notify{Prefs: prefs})

	cli.Login(nil, controlclient.LoginDefault)
	return nil
}

func (b *LocalBackend) updateFilter(netMap *controlclient.NetworkMap) {
	// TODO(apenwarr): don't replace filter at all if unchanged.
	// TODO(apenwarr): print a diff instead of full filter.
	if netMap == nil {
		// Not configured yet, block everything
		b.logf("netmap packet filter: (not ready yet)")
		b.e.SetFilter(filter.NewAllowNone())
	} else if b.Prefs().ShieldsUp {
		// Shields up, block everything
		b.logf("netmap packet filter: (shields up)")
		b.e.SetFilter(filter.NewAllowNone())
	} else {
		now := time.Now()
		if now.Sub(b.lastFilterPrint) > 1*time.Minute {
			b.logf("netmap packet filter: %v", netMap.PacketFilter)
			b.lastFilterPrint = now
		} else {
			b.logf("netmap packet filter: (length %d)", len(netMap.PacketFilter))
		}
		b.e.SetFilter(filter.New(netMap.PacketFilter, b.e.GetFilter()))
	}
}

func (b *LocalBackend) readPoller() {
	for {
		ports, ok := <-b.portpoll.C
		if !ok {
			return
		}
		sl := []tailcfg.Service{}
		for _, p := range ports {
			s := tailcfg.Service{
				Proto:       tailcfg.ServiceProto(p.Proto),
				Port:        p.Port,
				Description: p.Process,
			}
			if policy.IsInterestingService(s, version.OS()) {
				sl = append(sl, s)
			}
		}

		b.mu.Lock()
		if b.hiCache == nil {
			// TODO(bradfitz): it's a little weird that this port poller
			// is started (by NewLocalBackend) before the Start call.
			b.hiCache = new(tailcfg.Hostinfo)
		}
		b.hiCache.Services = sl
		hi := b.hiCache
		b.mu.Unlock()

		b.doSetHostinfoFilterServices(hi)
	}
}

func (b *LocalBackend) send(n Notify) {
	b.mu.Lock()
	notify := b.notify
	b.mu.Unlock()

	if notify != nil {
		n.Version = version.LONG
		notify(n)
	}
}

func (b *LocalBackend) popBrowserAuthNow() {
	b.mu.Lock()
	url := b.authURL
	b.interact = 0
	b.authURL = ""
	b.mu.Unlock()

	b.logf("popBrowserAuthNow: url=%v", url != "")

	b.blockEngineUpdates(true)
	b.stopEngineAndWait()
	b.send(Notify{BrowseToURL: &url})
	if b.State() == Running {
		b.enterState(Starting)
	}
}

// b.mu must be held
func (b *LocalBackend) loadStateLocked(key StateKey, prefs *Prefs, legacyPath string) error {
	if prefs == nil && key == "" {
		panic("state key and prefs are both unset")
	}

	if key == "" {
		// Frontend fully owns the state, we just need to obey it.
		b.logf("Using frontend prefs")
		b.prefs = prefs.Clone()
		b.stateKey = ""
		return nil
	}

	if prefs != nil {
		// Backend owns the state, but frontend is trying to migrate
		// state into the backend.
		b.logf("Importing frontend prefs into backend store")
		if err := b.store.WriteState(key, prefs.ToBytes()); err != nil {
			return fmt.Errorf("store.WriteState: %v", err)
		}
	}

	b.logf("Using backend prefs")
	bs, err := b.store.ReadState(key)
	if err != nil {
		if errors.Is(err, ErrStateNotExist) {
			if legacyPath != "" {
				b.prefs, err = LoadPrefs(legacyPath, true)
				if err != nil {
					b.logf("Failed to load legacy prefs: %v", err)
					b.prefs = NewPrefs()
				} else {
					b.logf("Imported state from relaynode for %q", key)
				}
			} else {
				b.prefs = NewPrefs()
				b.logf("Created empty state for %q", key)
			}
			b.stateKey = key
			return nil
		}
		return fmt.Errorf("store.ReadState(%q): %v", key, err)
	}
	b.prefs, err = PrefsFromBytes(bs, false)
	if err != nil {
		return fmt.Errorf("PrefsFromBytes: %v", err)
	}
	b.stateKey = key
	return nil
}

// State returns the backend's state.
func (b *LocalBackend) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.state
}

// EngineStatus returns the engine status. See also: Status, and State.
//
// TODO(bradfitz): deprecated this and merge it with the Status method
// that returns ipnstate.Status? Maybe have that take flags for what info
// the caller cares about?
func (b *LocalBackend) EngineStatus() EngineStatus {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.engineStatus
}

func (b *LocalBackend) StartLoginInteractive() {
	b.mu.Lock()
	b.assertClientLocked()
	b.interact++
	url := b.authURL
	c := b.c
	b.mu.Unlock()
	b.logf("StartLoginInteractive: url=%v", url != "")

	if url != "" {
		b.popBrowserAuthNow()
	} else {
		c.Login(nil, controlclient.LoginInteractive)
	}
}

func (b *LocalBackend) FakeExpireAfter(x time.Duration) {
	b.logf("FakeExpireAfter: %v", x)
	if b.netMapCache != nil {
		e := b.netMapCache.Expiry
		if e.IsZero() || time.Until(e) > x {
			b.netMapCache.Expiry = time.Now().Add(x)
		}
		b.send(Notify{NetMap: b.netMapCache})
	}
}

func (b *LocalBackend) LocalAddrs() []wgcfg.CIDR {
	if b.netMapCache != nil {
		return b.netMapCache.Addresses
	} else {
		return nil
	}
}

func (b *LocalBackend) Expiry() time.Time {
	if b.netMapCache != nil {
		return b.netMapCache.Expiry
	} else {
		return time.Time{}
	}
}

func (b *LocalBackend) parseWgStatus(s *wgengine.Status) EngineStatus {
	var ss []string
	var rx, tx wgengine.ByteCount
	peers := make(map[tailcfg.NodeKey]wgengine.PeerStatus)

	live := 0
	for _, p := range s.Peers {
		if p.LastHandshake.IsZero() {
			ss = append(ss, "x")
		} else {
			ss = append(ss, fmt.Sprintf("%d/%d", p.RxBytes, p.TxBytes))
			live++
			peers[p.NodeKey] = p
		}
		rx += p.RxBytes
		tx += p.TxBytes
	}
	b.logf("v%v peers: %v", version.LONG, strings.Join(ss, " "))
	return EngineStatus{
		RBytes:    rx,
		WBytes:    tx,
		NumLive:   live,
		LiveDERPs: s.DERPs,
		LivePeers: peers,
	}
}

func (b *LocalBackend) AdminPageURL() string {
	return b.serverURL + "/admin/machines"
}

func (b *LocalBackend) Prefs() *Prefs {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.prefs
}

func (b *LocalBackend) SetPrefs(new *Prefs) {
	if new == nil {
		panic("SetPrefs got nil prefs")
	}

	b.mu.Lock()
	old := b.prefs
	new.Persist = old.Persist // caller isn't allowed to override this
	b.prefs = new
	if b.stateKey != "" {
		if err := b.store.WriteState(b.stateKey, b.prefs.ToBytes()); err != nil {
			b.logf("Failed to save new controlclient state: %v", err)
		}
	}
	oldHi := b.hiCache
	newHi := oldHi.Clone()
	newHi.RoutableIPs = append([]wgcfg.CIDR(nil), b.prefs.AdvertiseRoutes...)
	b.hiCache = newHi
	b.mu.Unlock()

	b.logf("SetPrefs: %v", new.Pretty())

	if old.ShieldsUp != new.ShieldsUp || !oldHi.Equal(newHi) {
		b.doSetHostinfoFilterServices(newHi)
	}

	b.updateFilter(b.netMapCache)

	if old.WantRunning != new.WantRunning {
		b.stateMachine()
	} else {
		b.authReconfig()
	}

	b.send(Notify{Prefs: new})
}

func (b *LocalBackend) doSetHostinfoFilterServices(hi *tailcfg.Hostinfo) {
	hi2 := *hi
	prefs := b.Prefs()
	if prefs != nil && prefs.ShieldsUp {
		// No local services are available, since ShieldsUp will block
		// them all.
		hi2.Services = []tailcfg.Service{}
	}

	b.mu.Lock()
	cli := b.c
	b.mu.Unlock()

	// b.c might not be started yet
	if cli != nil {
		cli.SetHostinfo(&hi2)
	}
}

// Note: return value may be nil, if we haven't received a netmap yet.
func (b *LocalBackend) NetMap() *controlclient.NetworkMap {
	return b.netMapCache
}

func (b *LocalBackend) blockEngineUpdates(block bool) {
	// TODO(apenwarr): probably need mutex here (and several other places)
	b.logf("blockEngineUpdates(%v)", block)

	b.mu.Lock()
	b.blocked = block
	b.mu.Unlock()
}

func (b *LocalBackend) authReconfig() {
	b.mu.Lock()
	blocked := b.blocked
	uc := b.prefs
	nm := b.netMapCache
	b.mu.Unlock()

	if blocked {
		b.logf("authReconfig: blocked, skipping.")
		return
	}
	if nm == nil {
		b.logf("authReconfig: netmap not yet valid. Skipping.")
		return
	}
	if !uc.WantRunning {
		b.logf("authReconfig: skipping because !WantRunning.")
		return
	}

	uflags := controlclient.UDefault
	if uc.RouteAll {
		uflags |= controlclient.UAllowDefaultRoute
		// TODO(apenwarr): Make subnet routes a different pref?
		uflags |= controlclient.UAllowSubnetRoutes
		// TODO(apenwarr): Remove this once we sort out subnet routes.
		//  Right now default routes are broken in Windows, but
		//  controlclient doesn't properly send subnet routes. So
		//  let's convert a default route into a subnet route in order
		//  to allow experimentation.
		uflags |= controlclient.UHackDefaultRoute
	}
	if uc.AllowSingleHosts {
		uflags |= controlclient.UAllowSingleHosts
	}

	dns := nm.DNS
	dom := nm.DNSDomains
	if !uc.CorpDNS {
		dns = []wgcfg.IP{}
		dom = []string{}
	}
	cfg, err := nm.WGCfg(uflags, dns)
	if err != nil {
		log.Fatalf("WGCfg: %v", err)
	}

	err = b.e.Reconfig(cfg, dom)
	if err == wgengine.ErrNoChanges {
		return
	}
	b.logf("authReconfig: ra=%v dns=%v 0x%02x: %v", uc.RouteAll, uc.CorpDNS, uflags, err)
}

func (b *LocalBackend) enterState(newState State) {
	b.mu.Lock()
	state := b.state
	prefs := b.prefs
	notify := b.notify
	b.mu.Unlock()

	if state == newState {
		return
	}
	b.logf("Switching ipn state %v -> %v (WantRunning=%v)",
		state, newState, prefs.WantRunning)
	if notify != nil {
		b.send(Notify{State: &newState})
	}

	b.state = newState
	switch newState {
	case NeedsLogin:
		b.blockEngineUpdates(true)
		fallthrough
	case Stopped:
		err := b.e.Reconfig(&wgcfg.Config{}, nil)
		if err != nil {
			b.logf("Reconfig(down): %v", err)
		}
	case Starting, NeedsMachineAuth:
		b.authReconfig()
		// Needed so that UpdateEndpoints can run
		b.e.RequestStatus()
	case Running:
		break
	default:
		b.logf("[unexpected] unknown newState %#v", newState)
	}

}

func (b *LocalBackend) nextState() State {
	b.mu.Lock()
	b.assertClientLocked()
	var (
		c           = b.c
		netMap      = b.netMapCache
		state       = b.state
		wantRunning = b.prefs.WantRunning
	)
	b.mu.Unlock()

	if netMap == nil {
		if c.AuthCantContinue() {
			// Auth was interrupted or waiting for URL visit,
			// so it won't proceed without human help.
			return NeedsLogin
		} else {
			// Auth or map request needs to finish
			return state
		}
	} else if !wantRunning {
		return Stopped
	} else if e := netMap.Expiry; !e.IsZero() && time.Until(e) <= 0 {
		return NeedsLogin
	} else if netMap.MachineStatus != tailcfg.MachineAuthorized {
		// TODO(crawshaw): handle tailcfg.MachineInvalid
		return NeedsMachineAuth
	} else if state == NeedsMachineAuth {
		// (if we get here, we know MachineAuthorized == true)
		return Starting
	} else if state == Starting {
		if st := b.EngineStatus(); st.NumLive > 0 || st.LiveDERPs > 0 {
			return Running
		} else {
			return state
		}
	} else if state == Running {
		return Running
	} else {
		return Starting
	}
}

func (b *LocalBackend) RequestEngineStatus() {
	b.e.RequestStatus()
}

func (b *LocalBackend) RequestStatus() {
	st := b.Status()
	b.notify(Notify{Status: st})
}

// TODO(apenwarr): use a channel or something to prevent re-entrancy?
//  Or maybe just call the state machine from fewer places.
func (b *LocalBackend) stateMachine() {
	b.enterState(b.nextState())
}

func (b *LocalBackend) stopEngineAndWait() {
	b.logf("stopEngineAndWait...")
	b.e.Reconfig(&wgcfg.Config{}, nil)
	b.requestEngineStatusAndWait()
	b.logf("stopEngineAndWait: done.")
}

// Requests the wgengine status, and does not return until the status
// was delivered (to the usual callback).
func (b *LocalBackend) requestEngineStatusAndWait() {
	b.logf("requestEngineStatusAndWait")

	b.statusLock.Lock()
	go b.e.RequestStatus()
	b.logf("requestEngineStatusAndWait: waiting...")
	b.statusChanged.Wait() // temporarily releases lock while waiting
	b.logf("requestEngineStatusAndWait: got status update.")
	b.statusLock.Unlock()
}

// NOTE(apenwarr): No easy way to persist logged-out status.
//  Maybe that's for the better; if someone logs out accidentally,
//  rebooting will fix it.
func (b *LocalBackend) Logout() {
	b.mu.Lock()
	b.assertClientLocked()
	c := b.c
	b.netMapCache = nil
	b.mu.Unlock()

	c.Logout()

	b.mu.Lock()
	b.netMapCache = nil
	b.mu.Unlock()

	b.stateMachine()
}

func (b *LocalBackend) assertClientLocked() {
	if b.c == nil {
		panic("LocalBackend.assertClient: b.c == nil")
	}
}

func (b *LocalBackend) SetNetInfo(ni *tailcfg.NetInfo) {
	b.mu.Lock()
	c := b.c
	if b.hiCache != nil {
		b.hiCache.NetInfo = ni.Clone()
	}
	b.mu.Unlock()

	if c == nil {
		return
	}
	c.SetNetInfo(ni)
}
