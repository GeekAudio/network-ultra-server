package room

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestConcurrentCreateRechecksNameAfterHash(t *testing.T) {
	r := NewRegistry(10, 8)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Create("Same", "private", nil)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	success, taken := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrRoomNameTaken):
			taken++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if success != 1 || taken != 1 || r.CountRooms() != 1 {
		t.Fatalf("success=%d taken=%d rooms=%d", success, taken, r.CountRooms())
	}
}

func TestForEachPeerReleasesLockBeforeCallback(t *testing.T) {
	r := &Room{peers: make(map[uuid.UUID]*Peer)}
	p := NewPeer("one", "send")
	r.peers[p.ID] = p
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		r.ForEachPeer(func(*Peer) {
			close(entered)
			<-release
		})
		close(done)
	}()
	<-entered
	removed := make(chan struct{})
	go func() { r.Remove(p.ID); close(removed) }()
	select {
	case <-removed:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Remove blocked by ForEachPeer callback")
	}
	close(release)
	<-done
}

func TestPublicListDoesNotHoldRegistryLockWhileWaitingForRoom(t *testing.T) {
	r := NewRegistry(10, 8)
	rm, err := r.Create("public", "public", nil)
	if err != nil {
		t.Fatal(err)
	}
	rm.mu.Lock()
	listDone := make(chan struct{})
	go func() {
		_ = r.PublicList()
		close(listDone)
	}()

	// Give PublicList time to snapshot registry state and block on rm.mu.
	time.Sleep(20 * time.Millisecond)
	created := make(chan error, 1)
	go func() {
		_, createErr := r.Create("other", "private", nil)
		created <- createErr
	}()
	select {
	case createErr := <-created:
		if createErr != nil {
			t.Fatal(createErr)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("PublicList held registry lock while waiting for room lock")
	}
	rm.mu.Unlock()
	select {
	case <-listDone:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("PublicList did not finish after room lock release")
	}
}

func TestScheduleDestroyRequiresRoomStillEmpty(t *testing.T) {
	r := NewRegistry(10, 8)
	rm, err := r.Create("active", "private", nil)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPeer("joined", "send")
	if _, err := rm.Add(p); err != nil {
		t.Fatal(err)
	}
	rm.scheduleDestroyIfEmpty(time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	if r.Find("active") != rm || rm.PeerCount() != 1 || rm.destroyTimer != nil {
		t.Fatal("non-empty room armed or completed an empty-room destroy")
	}
}

func TestRemoveThenAddBeforeScheduleCannotDestroyActiveRoom(t *testing.T) {
	r := NewRegistry(10, 8)
	rm, err := r.Create("rejoin", "private", nil)
	if err != nil {
		t.Fatal(err)
	}
	first := NewPeer("first", "send")
	if _, err := rm.Add(first); err != nil {
		t.Fatal(err)
	}

	// Model the original Remove->schedule window deterministically: the last
	// peer is gone, a new peer joins, then the stale scheduling attempt runs.
	rm.mu.Lock()
	delete(rm.peers, first.ID)
	rm.mu.Unlock()
	first.setRoom(nil)
	second := NewPeer("second", "recv")
	if _, err := rm.Add(second); err != nil {
		t.Fatal(err)
	}
	rm.scheduleDestroyIfEmpty(time.Millisecond)
	time.Sleep(20 * time.Millisecond)

	if r.Find("rejoin") != rm || second.CurrentRoom() != rm || rm.PeerCount() != 1 {
		t.Fatal("stale empty-room schedule destroyed a room after a peer rejoined")
	}
	select {
	case <-rm.closeCh:
		t.Fatal("active room forwarder was closed")
	default:
	}
}

func TestDestroyCallbackRacingAddCannotOrphanActivePeer(t *testing.T) {
	r := NewRegistry(10, 8)
	rm, err := r.Create("callback-race", "private", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rm.destroyTimer != nil {
		rm.destroyTimer.Stop()
		rm.destroyTimer = nil
	}
	timer := time.AfterFunc(time.Hour, func() {})
	defer timer.Stop()
	rm.destroyTimer = timer
	rm.destroyGeneration++
	generation := rm.destroyGeneration

	registryLocked := make(chan struct{})
	releaseDestroy := make(chan struct{})
	r.beforeDestroyRoomLock = func() {
		close(registryLocked)
		<-releaseDestroy
	}
	destroyDone := make(chan struct{})
	go func() {
		r.destroy(rm, generation)
		close(destroyDone)
	}()
	<-registryLocked

	peer := NewPeer("racer", "recv")
	addDone := make(chan error, 1)
	go func() {
		_, addErr := rm.Add(peer)
		addDone <- addErr
	}()
	close(releaseDestroy)
	<-destroyDone
	addErr := <-addDone

	if addErr == nil {
		if r.Find("callback-race") != rm || peer.CurrentRoom() != rm || rm.PeerCount() != 1 {
			t.Fatal("successful racing Add joined a room removed from the registry")
		}
		select {
		case <-rm.closeCh:
			t.Fatal("successful racing Add lost the forwarder")
		default:
		}
		return
	}
	if !errors.Is(addErr, ErrRoomNotFound) {
		t.Fatalf("racing Add error=%v, want nil or ErrRoomNotFound", addErr)
	}
	if r.Find("callback-race") != nil || peer.CurrentRoom() != nil {
		t.Fatal("failed racing Add left a peer attached to a destroyed room")
	}
	select {
	case <-rm.closeCh:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("destroyed room forwarder did not close")
	}
}

func TestFanOutDropsQueuedFrameAfterSourceLeaves(t *testing.T) {
	r := NewRegistry(10, 8)
	rm, err := r.Create("leave-before-fanout", "private", nil)
	if err != nil {
		t.Fatal(err)
	}
	source := NewPeer("source", "send")
	receiver := NewPeer("receiver", "recv")
	if _, err := rm.Add(source); err != nil {
		t.Fatal(err)
	}
	if _, err := rm.Add(receiver); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	received := make(chan string, 2)
	receiver.AttachSender(func(payload []byte, binary bool) error {
		if !binary {
			return nil
		}
		if string(payload) == "block" {
			close(entered)
			<-release
		}
		received <- string(payload)
		return nil
	})
	if !rm.Forward(&Frame{SourcePeerID: source.ID, Payload: []byte("block")}) {
		t.Fatal("could not enqueue blocking frame")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("forwarder did not enter blocking send")
	}
	staleDone := make(chan struct{})
	if !rm.Forward(&Frame{
		SourcePeerID: source.ID,
		Payload:      []byte("stale"),
		Done:         func() { close(staleDone) },
	}) {
		t.Fatal("could not enqueue stale frame")
	}
	rm.Remove(source.ID)
	close(release)
	select {
	case <-staleDone:
	case <-time.After(time.Second):
		t.Fatal("stale queued frame was not released")
	}
	select {
	case got := <-received:
		if got != "block" {
			t.Fatalf("first payload=%q want block", got)
		}
	case <-time.After(time.Second):
		t.Fatal("blocking frame was not delivered")
	}
	select {
	case got := <-received:
		t.Fatalf("frame from departed source escaped: %q", got)
	default:
	}
}
