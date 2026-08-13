package room

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	ErrRoomNameTaken = errors.New("room name taken")
	ErrServerFull    = errors.New("max rooms reached")
	ErrRoomNotFound  = errors.New("room not found")
)

type Registry struct {
	maxRooms        int
	maxPeersPerRoom int

	mu    sync.RWMutex
	rooms map[string]*Room // key = lowercased room name

	// notifySubscribers is called whenever the public room list changes.
	notifyCb func(delta RoomListDelta)

	// beforeDestroyRoomLock is a deterministic race-test hook. Production
	// leaves it nil; tests use it to pause after registry locking but before
	// destroy obtains room.mu.
	beforeDestroyRoomLock func()
}

type RoomListDelta struct {
	Added   []RoomListEntry
	Removed []string
	Updated []RoomListEntry
}

type RoomListEntry struct {
	RoomName    string
	PeerCount   int
	MaxPeers    int
	HasPassword bool
	CreatedAt   time.Time
}

func NewRegistry(maxRooms, maxPeersPerRoom int) *Registry {
	return &Registry{
		maxRooms:        maxRooms,
		maxPeersPerRoom: maxPeersPerRoom,
		rooms:           make(map[string]*Room),
	}
}

// SetDeltaListener installs a single callback for public-room list changes.
// In v1 the WS layer registers one listener that fans out to subscribers.
func (r *Registry) SetDeltaListener(cb func(RoomListDelta)) {
	r.mu.Lock()
	r.notifyCb = cb
	r.mu.Unlock()
}

// Create attempts to create a room.
func (r *Registry) Create(name, visibility string, passwordHash []byte) (*Room, error) {
	if name == "" {
		return nil, errors.New("empty name")
	}
	nameLower := strings.ToLower(name)

	r.mu.Lock()
	// Callers perform password hashing before entering this method. Recheck
	// invariants only under the registry mutex to resolve concurrent creates.
	if _, exists := r.rooms[nameLower]; exists {
		r.mu.Unlock()
		return nil, ErrRoomNameTaken
	}
	if len(r.rooms) >= r.maxRooms {
		r.mu.Unlock()
		return nil, ErrServerFull
	}

	room := &Room{
		ID:           uuid.New(),
		Name:         name,
		NameLower:    nameLower,
		Visibility:   visibility,
		HasPassword:  passwordHash != nil,
		passwordHash: passwordHash,
		MaxPeers:     r.maxPeersPerRoom,
		CreatedAt:    time.Now(),
		peers:        make(map[uuid.UUID]*Peer),
		audioCh:      make(chan *Frame, audioChBuffer),
		closeCh:      make(chan struct{}),
		registry:     r,
	}
	r.rooms[nameLower] = room
	cb := r.notifyCb
	r.mu.Unlock()

	room.start()
	room.startIdleDestroyTimer()

	if visibility == "public" && cb != nil {
		cb(RoomListDelta{Added: []RoomListEntry{entryOf(room)}})
	}
	return room, nil
}

// Find looks up a room by name (case-insensitive).
func (r *Registry) Find(name string) *Room {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.rooms[strings.ToLower(name)]
}

// destroy removes an empty room only when timer is still the room's active
// destroy timer. Registry then room is the global lock order. Marking the room
// destroyed under both locks makes a racing Add either win (and cancel this
// deletion) or fail with ErrRoomNotFound; it can never join an orphaned room
// whose forwarder is being closed.
func (r *Registry) destroy(room *Room, generation uint64) {
	r.mu.Lock()
	existing, ok := r.rooms[room.NameLower]
	if !ok || existing != room {
		r.mu.Unlock()
		return
	}
	if r.beforeDestroyRoomLock != nil {
		r.beforeDestroyRoomLock()
	}
	room.mu.Lock()
	if room.destroyed || room.destroyTimer == nil || room.destroyGeneration != generation || len(room.peers) != 0 {
		room.mu.Unlock()
		r.mu.Unlock()
		return
	}
	room.destroyed = true
	room.destroyTimer = nil
	room.destroyGeneration++
	delete(r.rooms, room.NameLower)
	cb := r.notifyCb
	visibility := room.Visibility
	name := room.Name
	room.mu.Unlock()
	r.mu.Unlock()

	room.close()

	if visibility == "public" && cb != nil {
		cb(RoomListDelta{Removed: []string{name}})
	}
}

// PublicList returns a snapshot of all public rooms for room_list_result.
func (r *Registry) PublicList() []RoomListEntry {
	r.mu.RLock()
	rooms := make([]*Room, 0, len(r.rooms))
	for _, rm := range r.rooms {
		if rm.Visibility == "public" {
			rooms = append(rooms, rm)
		}
	}
	r.mu.RUnlock()
	out := make([]RoomListEntry, 0, len(rooms))
	for _, rm := range rooms {
		out = append(out, entryOf(rm))
	}
	return out
}

// entryOf builds a list entry without holding the registry lock. It may take
// the room lock to snapshot peer count, preserving registry→room lock order.
func entryOf(rm *Room) RoomListEntry {
	return RoomListEntry{
		RoomName:    rm.Name,
		PeerCount:   rm.PeerCount(),
		MaxPeers:    rm.MaxPeers,
		HasPassword: rm.HasPassword,
		CreatedAt:   rm.CreatedAt,
	}
}

// PublishUpdate fires an "updated" delta. Used by WS layer when peer count
// changes in a public room.
func (r *Registry) PublishUpdate(rm *Room) {
	r.mu.RLock()
	cb := r.notifyCb
	pub := rm.Visibility == "public"
	r.mu.RUnlock()
	if pub && cb != nil {
		cb(RoomListDelta{Updated: []RoomListEntry{entryOf(rm)}})
	}
}

// CountRooms returns the current room count.
func (r *Registry) CountRooms() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.rooms)
}

// CountPeers returns total peers across all rooms (used for /healthz).
func (r *Registry) CountPeers() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, rm := range r.rooms {
		n += rm.PeerCount()
	}
	return n
}
