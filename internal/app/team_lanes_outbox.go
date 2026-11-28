package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/scimux/scimux/internal/team"
)

func (o *shareOutbox) reconcileLanes(ctx context.Context, entries []team.Entry, owner string, nodes map[string]nodeSnap) {
	lanes, _ := team.LoadLaneShares(o.dataDir)
	if len(lanes) == 0 {
		return
	}
	seen := map[string][]team.Channel{}
	for _, lane := range lanes {
		o.reconcileLane(ctx, entries, owner, nodes, lane.ID, seen)
	}
}
func (o *shareOutbox) reconcileLane(ctx context.Context, entries []team.Entry, owner string, nodes map[string]nodeSnap, id string, seen map[string][]team.Channel) {
	if o.shareOps != nil {
		o.shareOps.Lock()
		defer o.shareOps.Unlock()
	}
	lane, shares, ok := o.freshLane(id)
	if !ok {
		return
	}
	entry, found := serverFor(entries, lane.Server)
	if !found {
		o.dropLane(lane.ID, shares)
		return
	}
	if o.waiting(entry.ID) || (lane.State != "stopping" && lane.State != "active") {
		return
	}
	if lane.State == "stopping" {
		o.stopLane(ctx, entry, owner, lane, shares)
		return
	}
	shares, lane, ok = o.enrollLane(ctx, entry, owner, lane, nodes, shares, seen)
	if !ok || !o.pushLaneMembers(ctx, entry, owner, lane, shares) {
		return
	}
	if lane.IndexRev == lane.Rev && o.laneDue(lane) {
		next, kept := o.noteLeavers(ctx, entry, owner, lane, seen)
		if !kept {
			return
		}
		lane = next
	}
	o.publishLane(ctx, entry, owner, lane, shares, nodes)
}
func (o *shareOutbox) freshLane(id string) (lane team.LaneShare, shares []team.Share, found bool) {
	err := o.withFiles(func() error {
		lanes, loadErr := team.LoadLaneShares(o.dataDir)
		if loadErr != nil {
			return loadErr
		}
		shares, loadErr = team.LoadShares(o.dataDir)
		if loadErr != nil {
			return loadErr
		}
		for _, item := range lanes {
			if item.ID == id {
				lane, found = item, true
			}
		}
		return nil
	})
	if err != nil {
		return team.LaneShare{}, nil, false
	}
	return lane, shares, found
}
func (o *shareOutbox) dropLane(id string, shares []team.Share) {
	for _, s := range shares {
		if s.Lane == id {
			o.deleteStored(s.UID)
		}
	}
	_ = o.removeLane(id)
}
func (o *shareOutbox) removeLane(id string) error {
	return o.withFiles(func() error { return team.RemoveLaneShare(o.dataDir, id) })
}
func (o *shareOutbox) stopLane(ctx context.Context, entry team.Entry, owner string, lane team.LaneShare, shares []team.Share) {
	for _, s := range shares {
		if s.Lane == lane.ID && s.State != "stopping" && o.mutateShare(s.UID, func(cur *team.Share) { cur.State = "stopping" }) != nil {
			return
		}
	}
	for _, s := range shares {
		if s.Lane == lane.ID {
			return
		}
	}
	if !o.open(ctx, entry) {
		return
	}
	err := o.client.DeleteChannel(ctx, entry.URL, entry.Token, owner, lane.ID, lane.Generation)
	gone := err == nil || errors.Is(err, team.ErrNoChannel) || errors.Is(err, team.ErrStaleGeneration) || errors.Is(err, team.ErrUnauthorized)
	if gone && o.removeLane(lane.ID) == nil {
		o.noteSuccess(entry.ID)
		return
	}
	if !gone {
		o.noteFailure(entry.ID, err)
	}
}
func (o *shareOutbox) enrollLane(ctx context.Context, entry team.Entry, owner string, lane team.LaneShare, nodes map[string]nodeSnap, shares []team.Share, seen map[string][]team.Channel) ([]team.Share, team.LaneShare, bool) {
	excluded := map[string]bool{}
	for _, id := range lane.Excluded {
		excluded[id] = true
	}
	byNode := map[string][]team.Share{}
	for _, s := range shares {
		byNode[s.Node] = append(byNode[s.Node], s)
	}
	refreshed := false
	for id, snap := range nodes {
		if !snap.live || snap.lane != lane.Lane || excluded[id] {
			continue
		}
		have := byNode[id]
		if len(have) == 1 && have[0].Lane == "" && have[0].Server == lane.Server && have[0].State == "active" {
			uid, _, _, ok := o.nodeCursor(id, false)
			if ok && uid == have[0].UID {
				if o.mutateShare(have[0].UID, func(cur *team.Share) { cur.Lane, cur.LaneRev, cur.Members = lane.ID, 0, []team.Member{} }) != nil {
					return shares, lane, false
				}
				for i := range shares {
					if shares[i].UID == have[0].UID {
						shares[i].Lane, shares[i].LaneRev, shares[i].Members = lane.ID, 0, []team.Member{}
					}
				}
			}
			continue
		}
		if len(have) != 0 {
			continue
		}
		uid, records, size, ok := o.nodeCursor(id, true)
		if !ok {
			continue
		}
		scope, start := "all", 0
		if n, known := lane.Starts[id]; known {
			if records <= n {
				o.keepLaneLook(id, size)
				continue
			}
			scope, start = "from_now", n
		}
		if lane.IndexRev == lane.Rev && !refreshed {
			next, kept := o.noteLeavers(ctx, entry, owner, lane, seen)
			if !kept {
				o.forgetLaneLook(id)
				return shares, lane, false
			}
			lane, refreshed = next, true
		}
		o.forgetLaneLook(id)
		saved, err := provisionChat(ctx, o.client, o.now, o.saveNewShare, entry, owner, openedShare{
			Node: id, UID: uid, Title: snap.title, Scope: scope, Start: start,
			Lane: lane.ID, LaneRev: lane.Rev, Members: []team.Member{}, Channel: lane.Members,
		})
		if err != nil {
			o.forgetLaneLook(id)
			o.noteFailure(entry.ID, err)
			return shares, lane, false
		}
		shares = append(shares, saved)
	}
	return shares, lane, true
}
func (o *shareOutbox) nodeCursor(node string, gate bool) (string, int, int64, bool) {
	size, err := o.fileSize(shareLogPath(o.dataDir, node))
	if err != nil {
		o.forgetLaneLook(node)
		return "", 0, 0, false
	}
	if gate && o.sameLaneLook(node, size) {
		return "", 0, size, false
	}
	uid, n, ok := o.readCursor(node)
	if gate && !ok {
		o.keepLaneLook(node, size)
	}
	return uid, n, size, ok
}
func (o *shareOutbox) sameLaneLook(node string, size int64) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	got, ok := o.laneLook[node]
	return ok && got == size
}
func (o *shareOutbox) keepLaneLook(node string, size int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.laneLook == nil {
		o.laneLook = map[string]int64{}
	}
	o.laneLook[node] = size
}
func (o *shareOutbox) forgetLaneLook(node string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.laneLook, node)
}
func (o *shareOutbox) readCursor(node string) (string, int, bool) {
	if o.cursor != nil {
		return o.cursor(node)
	}
	return (&teamHandler{dataDir: o.dataDir}).chatCursor(node)
}
func (o *shareOutbox) saveNewShare(s team.Share) error {
	return o.withFiles(func() error { return team.SaveShare(o.dataDir, s) })
}
func (o *shareOutbox) pushLaneMembers(ctx context.Context, entry team.Entry, owner string, lane team.LaneShare, shares []team.Share) bool {
	for _, s := range shares {
		if s.Lane != lane.ID || s.State != "active" || s.LaneRev >= lane.Rev {
			continue
		}
		if !o.setLaneMembers(ctx, entry, owner, s.UID, lane.Members) || o.mutateShare(s.UID, func(cur *team.Share) { cur.LaneRev = lane.Rev }) != nil {
			return false
		}
	}
	return lane.IndexRev >= lane.Rev || o.setLaneMembers(ctx, entry, owner, lane.ID, lane.Members) && o.mutateLane(lane.ID, func(cur *team.LaneShare) { cur.IndexRev = lane.Rev }) == nil
}
func (o *shareOutbox) setLaneMembers(ctx context.Context, entry team.Entry, owner, chat string, members []team.Member) bool {
	if !o.open(ctx, entry) {
		return false
	}
	err := o.client.SetMembers(ctx, entry.URL, entry.Token, owner, chat, members)
	if err != nil {
		o.noteFailure(entry.ID, err)
	}
	return err == nil
}
func (o *shareOutbox) noteLeavers(ctx context.Context, entry team.Entry, owner string, lane team.LaneShare, seen map[string][]team.Channel) (team.LaneShare, bool) {
	list, ok := o.channelsOnce(ctx, entry, seen)
	if !ok {
		return lane, false
	}
	on, have := []team.Member(nil), false
	for _, ch := range list {
		if ch.Owner == owner && ch.Chat == lane.ID {
			on, have = ch.Members, true
		}
	}
	next := append([]team.Member(nil), lane.Members...)
	if have {
		keep := next[:0]
		for _, m := range lane.Members {
			for _, got := range on {
				if got.Seat == m.Seat {
					keep = append(keep, m)
					break
				}
			}
		}
		next = keep
	}
	rev := lane.Rev
	if len(next) != len(lane.Members) {
		rev++
	}
	checked := o.now().UTC().Format(time.RFC3339)
	if o.mutateLane(lane.ID, func(cur *team.LaneShare) {
		cur.Members, cur.Rev, cur.CheckedAt = next, rev, checked
	}) != nil {
		return lane, false
	}
	lane.Members, lane.Rev, lane.CheckedAt = next, rev, checked
	return lane, true
}
func (o *shareOutbox) channelsOnce(ctx context.Context, entry team.Entry, seen map[string][]team.Channel) ([]team.Channel, bool) {
	if ch, ok := seen[entry.ID]; ok {
		return ch, true
	}
	if !o.open(ctx, entry) {
		return nil, false
	}
	ch, err := o.client.Channels(ctx, entry.URL, entry.Token, false)
	if err != nil {
		o.noteFailure(entry.ID, err)
		return nil, false
	}
	seen[entry.ID] = ch
	return ch, true
}
func (o *shareOutbox) laneDue(lane team.LaneShare) bool {
	if lane.CheckedAt == "" {
		return true
	}
	at, _ := time.Parse(time.RFC3339, lane.CheckedAt)
	return !o.now().Before(at.Add(time.Minute))
}
func (o *shareOutbox) publishLane(ctx context.Context, entry team.Entry, owner string, lane team.LaneShare, shares []team.Share, nodes map[string]nodeSnap) {
	body := laneSnapshotBody(lane, shares, nodes)
	hash := hex.EncodeToString(sha256Sum(body))
	if hash == lane.IndexHash || !o.open(ctx, entry) {
		return
	}
	gen, _, err := o.client.AppendMirror(ctx, entry.URL, entry.Token, owner, lane.ID, lane.Generation, []team.MirrorEntry{{
		Key: fmt.Sprintf("lane/%d", lane.IndexKey+1), Body: body,
	}})
	switch {
	case errors.Is(err, team.ErrStaleGeneration) || errors.Is(err, team.ErrNoChannel) || (err == nil && gen != lane.Generation):
		o.loseLane(entry.ID, lane.ID, "lost")
	case errors.Is(err, team.ErrQuota):
		o.loseLane(entry.ID, lane.ID, "full")
	case err != nil:
		o.noteFailure(entry.ID, err)
	default:
		if o.mutateLane(lane.ID, func(cur *team.LaneShare) { cur.IndexKey, cur.IndexHash = lane.IndexKey+1, hash }) == nil {
			o.noteSuccess(entry.ID)
		}
	}
}
func sha256Sum(body []byte) []byte {
	sum := sha256.Sum256(body)
	return sum[:]
}
func (o *shareOutbox) loseLane(server, id, state string) {
	_ = o.mutateLane(id, func(cur *team.LaneShare) { cur.State = state })
	o.clearConfirmed(server)
}
func laneSnapshotBody(lane team.LaneShare, shares []team.Share, nodes map[string]nodeSnap) []byte {
	type chat struct {
		Chat string `json:"chat"`
		At   string `json:"created_at"`
	}
	rows := make([]chat, 0)
	for _, s := range shares {
		if s.Lane != lane.ID || s.State != "active" {
			continue
		}
		at := s.SharedAt
		if snap, ok := nodes[s.Node]; ok && snap.live {
			at = snap.createdAt
		}
		rows = append(rows, chat{s.UID, at})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].At != rows[j].At {
			return rows[i].At < rows[j].At
		}
		return rows[i].Chat < rows[j].Chat
	})
	body, _ := json.Marshal(struct {
		V     int    `json:"v"`
		T     string `json:"t"`
		Name  string `json:"name"`
		Color string `json:"color"`
		Chats []chat `json:"chats"`
	}{1, "lane", lane.Name, lane.Color, rows})
	return body
}
func (o *shareOutbox) mutateShare(uid string, fn func(*team.Share)) error {
	return o.withFiles(func() error {
		shares, err := team.LoadShares(o.dataDir)
		if err != nil {
			return err
		}
		for i := range shares {
			if shares[i].UID != uid {
				continue
			}
			fn(&shares[i])
			return team.SaveShare(o.dataDir, shares[i])
		}
		return team.ErrNotFound
	})
}
func (o *shareOutbox) mutateLane(id string, fn func(*team.LaneShare)) error {
	return o.withFiles(func() error {
		lanes, err := team.LoadLaneShares(o.dataDir)
		if err != nil {
			return err
		}
		for i := range lanes {
			if lanes[i].ID != id {
				continue
			}
			fn(&lanes[i])
			return team.SaveLaneShare(o.dataDir, lanes[i])
		}
		return team.ErrNotFound
	})
}
