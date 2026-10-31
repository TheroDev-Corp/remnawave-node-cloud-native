package node

import (
	"encoding/json"
	"sync"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/hashedset"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/xray"
)

// Hashes is internals.hashes from xray/start.
type Hashes struct {
	EmptyConfig string `json:"emptyConfig"`
	Inbounds    []struct {
		UsersCount int    `json:"usersCount"`
		Hash       string `json:"hash"`
		Tag        string `json:"tag"`
	} `json:"inbounds"`
}

// orderedSet is a JS Set<string>: iteration in insertion order.
type orderedSet struct {
	order []string
	has   map[string]bool
}

func newOrderedSet() *orderedSet { return &orderedSet{has: map[string]bool{}} }

func (s *orderedSet) add(v string) {
	if !s.has[v] {
		s.has[v] = true
		s.order = append(s.order, v)
	}
}

func (s *orderedSet) del(v string) {
	if !s.has[v] {
		return
	}
	delete(s.has, v)
	for i, x := range s.order {
		if x == v {
			s.order = append(s.order[:i:i], s.order[i+1:]...)
			break
		}
	}
}

func (s *orderedSet) list() []string { return append([]string(nil), s.order...) }

// State is internal.service.ts: the generated config, the panel's hashes,
// one HashedSet per inbound, the known inbound tags, and the user
// definitions used to restore suspended users.
//
// Design fix vs the TS fork: a user definition is the list of xray
// operations that were actually applied (after the bulk path's base64 key
// encoding and cipher choice), not the request DTO. Restoring replays
// exactly what was live, so SS2022 users get their real key back.
type State struct {
	mu          sync.Mutex
	xrayConfig  map[string]any
	lastStart   json.RawMessage
	emptyHash   string
	inbounds    map[string]*hashedset.Set
	tags        *orderedSet
	definitions map[string]UserDef
}

// UserDef is what restoring a suspended user re-applies.
type UserDef struct {
	Ops  []xray.User
	Hash string // the vlessUuid tracked in the inbound hash sets
}

func NewState() *State {
	return &State{inbounds: map[string]*hashedset.Set{}, tags: newOrderedSet(), definitions: map[string]UserDef{}}
}

func (s *State) XrayConfig() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.xrayConfig
}

func (s *State) SetLastStart(body json.RawMessage) {
	s.mu.Lock()
	s.lastStart = body
	s.mu.Unlock()
}

func (s *State) LastStart() json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastStart
}

// ExtractUsers is extractUsersFromConfig: reset, remember the hashes, and
// build a HashedSet of client ids for every inbound the panel listed.
func (s *State) ExtractUsers(h Hashes, cfg map[string]any) {
	s.Cleanup(false)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emptyHash = h.EmptyConfig
	s.xrayConfig = cfg
	valid := map[string]bool{}
	for _, in := range h.Inbounds {
		valid[in.Tag] = true
	}
	inbounds, _ := cfg["inbounds"].([]any)
	for _, raw := range inbounds {
		in, _ := raw.(map[string]any)
		tag, _ := in["tag"].(string)
		if tag == "" || !valid[tag] {
			continue
		}
		set := hashedset.New()
		if settings, ok := in["settings"].(map[string]any); ok {
			if clients, ok := settings["clients"].([]any); ok {
				for _, c := range clients {
					if cm, ok := c.(map[string]any); ok {
						if id, ok := cm["id"].(string); ok && id != "" {
							set.Add(id)
						}
					}
				}
			}
		}
		s.inbounds[tag] = set
	}
	for _, raw := range inbounds { // tags in config order (pMap finishes in order for sync work)
		in, _ := raw.(map[string]any)
		if tag, _ := in["tag"].(string); s.inbounds[tag] != nil {
			s.tags.add(tag)
		}
	}
}

// NeedRestart is isNeedRestartCore.
func (s *State) NeedRestart(h Hashes) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.emptyHash == "" {
		return true, "no previous configuration"
	}
	if h.EmptyConfig != s.emptyHash {
		return true, "Detected changes in Xray Core base configuration"
	}
	if len(h.Inbounds) != len(s.inbounds) {
		return true, "Number of Xray Core inbounds has changed"
	}
	for tag, set := range s.inbounds {
		found := false
		for _, in := range h.Inbounds {
			if in.Tag != tag {
				continue
			}
			found = true
			if set.Hash64String() != in.Hash {
				return true, "User configuration changed for inbound " + tag + " (" + set.Hash64String() + " → " + in.Hash + ")"
			}
			break
		}
		if !found {
			return true, "Inbound " + tag + " no longer exists in Xray Core configuration"
		}
	}
	return false, ""
}

func (s *State) AddUserToInbound(tag, user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if set, ok := s.inbounds[tag]; ok {
		set.Add(user)
		return
	}
	s.inbounds[tag] = hashedset.New(user)
}

// RemoveUserFromInbound drops the inbound entirely once it is empty, which
// (as in the TS node) forces a restart on the next xray/start.
func (s *State) RemoveUserFromInbound(tag, user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, ok := s.inbounds[tag]
	if !ok {
		return
	}
	set.Delete(user)
	if set.Len() == 0 {
		s.tags.del(tag)
		delete(s.inbounds, tag)
	}
}

func (s *State) Tags() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tags.list()
}

func (s *State) AddTag(tag string) {
	s.mu.Lock()
	s.tags.add(tag)
	s.mu.Unlock()
}

func (s *State) StoreDefinition(user string, d UserDef) {
	s.mu.Lock()
	s.definitions[user] = d
	s.mu.Unlock()
}

func (s *State) Definition(user string) (UserDef, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.definitions[user]
	return d, ok
}

func (s *State) RemoveDefinition(user string) {
	s.mu.Lock()
	delete(s.definitions, user)
	s.mu.Unlock()
}

// Cleanup is internalService.cleanup().
func (s *State) Cleanup(clearLastStart bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if clearLastStart {
		s.lastStart = nil
	}
	s.inbounds = map[string]*hashedset.Set{}
	s.tags = newOrderedSet()
	s.definitions = map[string]UserDef{}
	s.xrayConfig = nil
	s.emptyHash = ""
}

// InboundHash exposes one inbound's current hash (tests, debugging).
func (s *State) InboundHash(tag string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, ok := s.inbounds[tag]
	if !ok {
		return "", false
	}
	return set.Hash64String(), true
}
