package kgo

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/twmb/kafka-go/pkg/kerr"
	"github.com/twmb/kafka-go/pkg/kmsg"
)

// GroupOpt is an option to configure group consuming.
type GroupOpt interface {
	apply(*groupConsumer)
}

// groupOpt implements GroupOpt.
type groupOpt struct {
	fn func(cfg *groupConsumer)
}

func (opt groupOpt) apply(cfg *groupConsumer) { opt.fn(cfg) }

// GroupTopics adds topics to use for group consuming.
func GroupTopics(topics ...string) GroupOpt {
	return groupOpt{func(cfg *groupConsumer) {
		cfg.topics = make(map[string]struct{}, len(topics))
		for _, topic := range topics {
			cfg.topics[topic] = struct{}{}
		}
	}}
}

// GroupTopicsRegex sets all topics in GroupTopics to be parsed as regular
// expressions.
func GroupTopicsRegex() GroupOpt {
	return groupOpt{func(cfg *groupConsumer) { cfg.regexTopics = true }}
}

// Balancers sets the balancer to use for dividing topic partitions among
// group members, overriding the defaults.
//
// The current default is [cooperative-sticky].
//
// For balancing, Kafka chooses the first protocol that all group members agree
// to support.
//
// Note that the current default of cooperative-sticky only means that this
// client will perform cooperative balancing, which is incompatible with eager
// balancing. To support an eager balancing strategy, be sure to override this
// option.
func Balancers(balancers ...GroupBalancer) GroupOpt {
	return groupOpt{func(cfg *groupConsumer) { cfg.balancers = balancers }}
}

// SessionTimeout sets how long a member the group can go between
// heartbeats, overriding the default 10,000ms. If a member does not heartbeat
// in this timeout, the broker will remove the member from the group and
// initiate a rebalance.
//
// This corresponds to Kafka's session.timeout.ms setting and must be within
// the broker's group.min.session.timeout.ms and group.max.session.timeout.ms.
func SessionTimeout(timeout time.Duration) GroupOpt {
	return groupOpt{func(cfg *groupConsumer) { cfg.sessionTimeout = timeout }}
}

// RebalanceTimeout sets how long group members are allowed to take
// when a JoinGroup is initiated (i.e., a rebalance has begun), overriding the
// default 60,000ms. This is essentially how long all members are allowed to
// complete work and commit offsets.
//
// Kafka uses the largest rebalance timeout of all members in the group. If a
// member does not rejoin within this timeout, Kafka will kick that member from
// the group.
//
// This corresponds to Kafka's rebalance.timeout.ms.
func RebalanceTimeout(timeout time.Duration) GroupOpt {
	return groupOpt{func(cfg *groupConsumer) { cfg.rebalanceTimeout = timeout }}
}

// HeartbeatInterval sets how long a group member goes between
// heartbeats to Kafka, overriding the default 3,000ms.
//
// Kafka uses heartbeats to ensure that a group member's session stays active.
// This value can be any value lower than the session timeout, but should be no
// higher than 1/3rd the session timeout.
//
// This corresponds to Kafka's heartbeat.interval.ms.
func HeartbeatInterval(interval time.Duration) GroupOpt {
	return groupOpt{func(cfg *groupConsumer) { cfg.heartbeatInterval = interval }}
}

// OnAssigned sets the function to be called when a group is joined after
// partitions are assigned before fetches begin.
//
// Note that this function combined with onRevoked should combined not exceed
// the rebalance interval. It is possible for the group, immediately after
// finishing a balance, to re-enter a new balancing session.
//
// The onAssigned function is passed the group's context, which is only
// canceled if the group is left or the client is closed.
func OnAssigned(onAssigned func(context.Context, map[string][]int32)) GroupOpt {
	return groupOpt{func(cfg *groupConsumer) { cfg.onAssigned = onAssigned }}
}

// OnRevoked sets the function to be called once a group transitions from
// stable to rebalancing.
//
// Note that this function combined with onAssigned should combined not exceed
// the rebalance interval. It is possible for the group, immediately after
// finishing a balance, to re-enter a new balancing session.
//
// If autocommit is enabled, the default onRevoked is to commit all offsets.
//
// The onRevoked function is passed the group's context, which is only canceled
// if the group is left or the client is closed.
func OnRevoked(onRevoked func(context.Context, map[string][]int32)) GroupOpt {
	return groupOpt{func(cfg *groupConsumer) { cfg.onRevoked = onRevoked }}
}

// OnLost sets the function to be called on "fatal" group errors, such as
// IllegalGeneration, UnknownMemberID, and authentication failures. This
// function differs from onRevoked in that it is unlikely that commits will
// succeed when partitions are outright lost, whereas commits likely will
// succeed when revoking partitions.
func OnLost(onLost func(context.Context, map[string][]int32)) GroupOpt {
	return groupOpt{func(cfg *groupConsumer) { cfg.onLost = onLost }}
}

// DisableAutoCommit disable auto committing.
func DisableAutoCommit() GroupOpt {
	return groupOpt{func(cfg *groupConsumer) { cfg.autocommitDisable = true }}
}

// AutoCommitInterval sets how long to go between autocommits, overriding the
// default 5s.
func AutoCommitInterval(interval time.Duration) GroupOpt {
	return groupOpt{func(cfg *groupConsumer) { cfg.autocommitInterval = interval }}
}

// InstanceID sets the group consumer's instance ID, switching the group
// member from "dynamic" to "static".
//
// Prior to Kafka 2.3.0, joining a group gave a group member a new member ID.
// The group leader could not tell if this was a rejoining member. Thus, any
// join caused the group to rebalance.
//
// Kafka 2.3.0 introduced the concept of an instance ID, which can persist
// across restarts. This allows for avoiding many costly rebalances and allows
// for stickier rebalancing for rejoining members (since the ID for balancing
// stays the same). The main downsides are that you, the user of a client, have
// to manage instance IDs properly, and that it may take longer to rebalance in
// the event that a client legitimately dies.
//
// When using an instance ID, the client does NOT send a leave group request
// when closing. This allows for the client ot restart with the same instance
// ID and rejoin the group to avoid a rebalance. It is strongly recommended to
// increase the session timeout enough to allow time for the restart (remember
// that the default session timeout is 10s).
//
// To actually leave the group, you must use an external admin commant that
// issues a leave group request on behalf of this instance ID (see kcl). If
// necessary, this package may introduce a manual leave command in the future.
func InstanceID(id string) GroupOpt {
	return groupOpt{func(cfg *groupConsumer) { cfg.instanceID = &id }}
}

type groupConsumer struct {
	c   *consumer // used to change consumer state; generally c.mu is grabbed on access
	cl  *Client   // used for running requests / adding to topics map
	seq uint64    // consumer's seq at time of Assign and after every fetch offsets

	ctx    context.Context
	cancel func()

	id          string
	topics      map[string]struct{}
	balancers   []GroupBalancer
	cooperative bool

	mu           sync.Mutex     // guards this block
	leader       bool           // whether we are the leader right now
	using        map[string]int // topics we are currently using => partitions known in that topic
	uncommitted  uncommitted
	commitCancel func()
	commitDone   chan struct{}

	rejoinCh chan struct{} // cap 1; sent to if subscription changes (regex)

	regexTopics bool
	reSeen      map[string]struct{}

	memberID     string
	instanceID   *string
	generation   int32
	lastAssigned map[string][]int32
	nowAssigned  map[string][]int32

	sessionTimeout    time.Duration
	rebalanceTimeout  time.Duration
	heartbeatInterval time.Duration

	onAssigned func(context.Context, map[string][]int32)
	onRevoked  func(context.Context, map[string][]int32)
	onLost     func(context.Context, map[string][]int32)

	blockAuto          bool
	autocommitDisable  bool
	autocommitInterval time.Duration

	offsetsAddedToTxn bool
}

// AssignGroup assigns a group to consume from, overriding any prior
// assignment. To leave a group, you can AssignGroup with an empty group.
// It is recommended to do one final syncronous commit before leaving a group.
func (cl *Client) AssignGroup(group string, opts ...GroupOpt) {
	// TODO rejoin existing group: revoke old partitions without leaving
	// and rejoining. See TODO in g.revoke.
	c := &cl.consumer
	c.mu.Lock()
	defer c.mu.Unlock()

	c.unassignPrior()

	ctx, cancel := context.WithCancel(cl.ctx)
	g := &groupConsumer{
		c:   c,
		cl:  cl,
		seq: c.seq,

		ctx:    ctx,
		cancel: cancel,

		id: group,

		balancers: []GroupBalancer{
			CooperativeStickyBalancer(),
		},
		cooperative: true,

		using:    make(map[string]int),
		rejoinCh: make(chan struct{}, 1),
		reSeen:   make(map[string]struct{}),

		sessionTimeout:    10000 * time.Millisecond,
		rebalanceTimeout:  60000 * time.Millisecond,
		heartbeatInterval: 3000 * time.Millisecond,

		autocommitInterval: 5 * time.Second,
	}
	if c.cl.cfg.txnID == nil {
		g.onRevoked = g.defaultRevoke
	} else {
		g.autocommitDisable = true
	}
	for _, opt := range opts {
		opt.apply(g)
	}
	if len(group) == 0 || len(g.topics) == 0 || c.dead {
		c.typ = consumerTypeUnset
		return
	}
	for _, balancer := range g.balancers {
		g.cooperative = g.cooperative && balancer.isCooperative()
	}
	c.typ = consumerTypeGroup
	c.group = g

	// Ensure all topics exist so that we will fetch their metadata.
	if !g.regexTopics {
		cl.topicsMu.Lock()
		clientTopics := cl.cloneTopics()
		for topic := range g.topics {
			if _, exists := clientTopics[topic]; !exists {
				clientTopics[topic] = newTopicPartitions(topic)
			}
		}
		cl.topics.Store(clientTopics)
		cl.topicsMu.Unlock()
	}

	if !g.autocommitDisable && g.autocommitInterval > 0 {
		go g.loopCommit()
	}

	cl.triggerUpdateMetadata()
}

func (g *groupConsumer) manage() {
	var consecutiveErrors int
loop:
	for {
		err := g.joinAndSync()
		if err == nil {
			if err = g.setupAssigned(); err != nil {
				if err == kerr.RebalanceInProgress {
					err = nil
				}
			}
		}

		if err != nil {
			if g.onLost != nil {
				g.onLost(g.ctx, g.nowAssigned)
			}
			consecutiveErrors++
			// Waiting for the backoff is a good time to update our
			// metadata; maybe the error is from stale metadata.
			backoff := g.cl.cfg.retryBackoff(consecutiveErrors)
			deadline := time.Now().Add(backoff)
			g.cl.waitmeta(g.ctx, backoff)
			after := time.NewTimer(time.Until(deadline))
			select {
			case <-g.ctx.Done():
				after.Stop()
				return
			case <-after.C:
				continue loop
			}
		}
		consecutiveErrors = 0
	}
}

func (g *groupConsumer) leave() {
	g.cancel()
	if g.instanceID == nil {
		g.cl.Request(g.cl.ctx, &kmsg.LeaveGroupRequest{
			Group:    g.id,
			MemberID: g.memberID,
			Members: []kmsg.LeaveGroupRequestMember{{
				MemberID: g.memberID,
			}},
		})
	}
}

func (g *groupConsumer) diffAssigned() (added, lost map[string][]int32) {
	if g.lastAssigned == nil {
		return g.nowAssigned, nil
	}

	added = make(map[string][]int32, len(g.nowAssigned))
	lost = make(map[string][]int32, len(g.nowAssigned))

	// First we loop over lastAssigned to find what was lost, or what was
	// added to topics we were working on.
	lasts := make(map[int32]struct{}, 100)
	for topic, lastPartitions := range g.lastAssigned {
		nowPartitions, exists := g.nowAssigned[topic]
		if !exists {
			lost[topic] = lastPartitions
			continue
		}

		for _, lastPartition := range lastPartitions {
			lasts[lastPartition] = struct{}{}
		}

		// Anything now that does not exist in last is new,
		// otherwise it is in common and we ignore it.
		for _, nowPartition := range nowPartitions {
			if _, exists := lasts[nowPartition]; !exists {
				added[topic] = append(added[topic], nowPartition)
			} else {
				delete(lasts, nowPartition)
			}
		}

		// Anything remanining in last does not exist now
		// and is thus lost.
		for last := range lasts {
			lost[topic] = append(lost[topic], last)
			delete(lasts, last) // reuse lasts
		}
	}

	// We loop again over nowAssigned to add entirely new topics to added.
	for topic, nowPartitions := range g.nowAssigned {
		if _, exists := g.lastAssigned[topic]; !exists {
			added[topic] = nowPartitions
		}
	}

	return added, lost
}

type revokeStage int8

const (
	revokeLastSession = iota
	revokeThisSession
)

// revoke calls onRevoked for partitions that this group member is losing and
// updates the uncommitted map after the revoke.
//
// For eager consumers, this simply revokes g.assigned. This will only be
// called at the end of a group session.
//
// For cooperative consumers, this either
//
//     (1) if revoking lost partitions from a prior session (i.e., after sync),
//         this revokes the passed in lost
//     (2) if revoking at the end of a session, this revokes topics that the
//         consumer is no longer interested in consuming (TODO, actually).
//
// Lastly, for cooperative consumers, this must selectively delete what was
// lost from the uncommitted map.
func (g *groupConsumer) revoke(stage revokeStage, lost map[string][]int32) {
	if !g.cooperative { // stage == revokeThisSession if not cooperative
		if g.onRevoked != nil {
			g.onRevoked(g.ctx, g.nowAssigned)
		}
		g.nowAssigned = nil
		g.mu.Lock()
		g.uncommitted = nil
		g.mu.Unlock()
		return
	}

	switch stage {
	case revokeLastSession:
		// we use lost in this case

	case revokeThisSession:
		// lost is nil for cooperative assigning. Instead, we determine
		// lost by finding subscriptions we are no longer interested in.
		//
		// TODO only relevant when we allow AssignGroup with the same
		// group to change subscriptions.
		//
		// Also, we must delete these partitions from nowAssigned.
	}

	if len(lost) == 0 { // if we lost nothing, do nothing
		return
	}

	// We must now stop fetching anything we lost and invalidate any
	// buffered fetches before falling into onRevoked.
	//
	// We want to invalidate buffered fetches since they may contain
	// partitions that we lost, and we do not want a future poll to
	// return those fetches. We could be smarter and knife out only
	// partitions we lost, but it is simpler to just drop everything.
	lostOffsets := make(map[string]map[int32]Offset, len(lost))
	for lostTopic, lostPartitions := range lost {
		lostPartitionOffsets := make(map[int32]Offset, len(lostPartitions))
		for _, lostPartition := range lostPartitions {
			lostPartitionOffsets[lostPartition] = Offset{}
		}
		lostOffsets[lostTopic] = lostPartitionOffsets
	}
	g.c.maybeAssignPartitions(&g.seq, lostOffsets, assignInvalidateMatching)

	if g.onRevoked != nil {
		g.onRevoked(g.ctx, lost)
	}

	defer g.rejoin()

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.uncommitted == nil {
		return
	}
	for lostTopic, lostPartitions := range lost {
		uncommittedPartitions := g.uncommitted[lostTopic]
		if uncommittedPartitions == nil {
			continue
		}
		for _, lostPartition := range lostPartitions {
			delete(uncommittedPartitions, lostPartition)
		}
		if len(uncommittedPartitions) == 0 {
			delete(g.uncommitted, lostTopic)
		}
	}
	if len(g.uncommitted) == 0 {
		g.uncommitted = nil
	}

}

// assignRevokeSession aids in sequencing prerevoke/assign/revoke.
type assignRevokeSession struct {
	prerevokeDone chan struct{}
	assignDone    chan struct{}
	revokeDone    chan struct{}
}

func newAssignRevokeSession() *assignRevokeSession {
	return &assignRevokeSession{
		prerevokeDone: make(chan struct{}),
		assignDone:    make(chan struct{}),
		revokeDone:    make(chan struct{}),
	}
}

func (s *assignRevokeSession) prerevoke(g *groupConsumer, lost map[string][]int32) <-chan struct{} {
	go func() {
		defer close(s.prerevokeDone)
		if g.cooperative {
			g.revoke(revokeLastSession, lost)
		}
	}()
	return s.prerevokeDone
}

func (s *assignRevokeSession) assign(g *groupConsumer, newAssigned map[string][]int32) <-chan struct{} {
	go func() {
		defer close(s.assignDone)
		<-s.prerevokeDone
		if g.onAssigned != nil {
			// We always call on assigned, even if nothing new is
			// assigned. This allows consumers to know that
			// assignment is done; transactional consumers can
			// reset offsets as necessary, for example.
			g.onAssigned(g.ctx, newAssigned)
		}
	}()
	return s.assignDone
}

func (s *assignRevokeSession) revoke(g *groupConsumer) <-chan struct{} {
	go func() {
		defer close(s.revokeDone)
		<-s.assignDone
		if g.onRevoked != nil {
			g.revoke(revokeThisSession, nil)
		}
	}()
	return s.revokeDone
}

func (g *groupConsumer) setupAssigned() error {
	hbErrCh := make(chan error, 1)
	fetchErrCh := make(chan error, 1)

	s := newAssignRevokeSession()
	added, lost := g.diffAssigned()
	s.prerevoke(g, lost)

	ctx, cancel := context.WithCancel(g.ctx)
	go func() {
		defer cancel() // potentially kill offset fetching
		hbErrCh <- g.heartbeat(fetchErrCh, s)
	}()

	select {
	case err := <-hbErrCh:
		return err
	case <-s.assign(g, added):
	}

	if len(added) > 0 {
		go func() { fetchErrCh <- g.fetchOffsets(ctx, added) }()
	} else {
		close(fetchErrCh)
	}

	return <-hbErrCh
}

// heartbeat issues heartbeat requests to Kafka for the duration of a group
// session.
//
// This function is began before fetching offsets to allow the consumer's
// onAssigned to be called before fetching. If the eventual offset fetch
// errors, we continue heartbeating until onRevoked finishes and our metadata
// is updated.
//
// If the offset fetch is successful, then we basically sit in this function
// until a heartbeat errors or us, being the leader, decides to re-join.
func (g *groupConsumer) heartbeat(fetchErrCh <-chan error, s *assignRevokeSession) error {
	ticker := time.NewTicker(g.heartbeatInterval)
	defer ticker.Stop()

	var metadone, revoked <-chan struct{}
	var didMetadone, didRevoke bool
	var lastErr error

	for {
		var err error
		select {
		case <-ticker.C:
			req := &kmsg.HeartbeatRequest{
				Group:      g.id,
				Generation: g.generation,
				MemberID:   g.memberID,
				InstanceID: g.instanceID,
			}
			var kresp kmsg.Response
			kresp, err = g.cl.Request(g.ctx, req)
			if err == nil {
				resp := kresp.(*kmsg.HeartbeatResponse)
				err = kerr.ErrorForCode(resp.ErrorCode)
			}
		case <-g.rejoinCh:
			// If a metadata update changes our subscription,
			// we just pretend we are rebalancing.
			err = kerr.RebalanceInProgress
		case err = <-fetchErrCh:
			fetchErrCh = nil
		case <-metadone:
			metadone = nil
			didMetadone = true
		case <-revoked:
			revoked = nil
			didRevoke = true
		case <-g.ctx.Done():
			<-s.assignDone // fall into onLost logic
			return errors.New("left group or client closed")
		}

		if didMetadone && didRevoke {
			return lastErr
		}

		if err == nil {
			continue
		}

		// Since we errored, we must revoke.
		if !didRevoke && revoked == nil {
			// If we are an eager consumer, we stop fetching all of
			// our current partitions as we will be revoking them.
			if !g.cooperative {
				g.c.maybeAssignPartitions(&g.seq, nil, assignInvalidateAll)
			}

			// If our error is not from rebalancing, then we
			// encountered IllegalGeneration or UnknownMemberID,
			// both of which are unexpected and unrecoverable.
			//
			// We return early rather than revoking and updating
			// metadata; the groupConsumer's manage function will
			// call onLost with all partitions.
			//
			// We still wait for the session's onAssigned to be
			// done so that we avoid calling onLost concurrently.
			if err != kerr.RebalanceInProgress {
				<-s.assignDone
				return err
			}

			// Now we call the user provided revoke callback, even
			// if cooperative: if cooperative, this only revokes
			// partitions we no longer want to consume.
			revoked = s.revoke(g)
		}
		// Since we errored, while waiting for the revoke to finish, we
		// update our metadata. A leader may have re-joined with new
		// metadata, and we want the update.
		if !didMetadone && metadone == nil {
			waited := make(chan struct{})
			metadone = waited
			go func() {
				g.cl.waitmeta(g.ctx, g.sessionTimeout)
				close(waited)
			}()
		}

		// We always save the latest error; generally this should be
		// REBALANCE_IN_PROGRESS, but if the revoke takes too long,
		// Kafka may boot us and we will get a different error.
		lastErr = err
	}
}

// We need to lock to set the leader due to the potential for a concurrent
// findNewAssignments.
func (g *groupConsumer) setLeader() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.leader = true
}

// prejoin, called at the beginning of joinAndSync, ensures we leave nothing
// uncommitted and that the rejoinCh is drained after a new join.
func (g *groupConsumer) prejoin() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.leader = false

	select {
	case <-g.rejoinCh:
	default:
	}
}

// rejoin is called if we are leader: this ensures the heartbeat loop will
// see we need to rejoin.
func (g *groupConsumer) rejoin() {
	select {
	case g.rejoinCh <- struct{}{}:
	default:
	}
}

func (g *groupConsumer) joinAndSync() error {
	g.prejoin()

start:
	req := kmsg.JoinGroupRequest{
		Group:                  g.id,
		SessionTimeoutMillis:   int32(g.sessionTimeout.Milliseconds()),
		RebalanceTimeoutMillis: int32(g.rebalanceTimeout.Milliseconds()),
		ProtocolType:           "consumer",
		MemberID:               g.memberID,
		InstanceID:             g.instanceID,
		Protocols:              g.joinGroupProtocols(),
	}
	kresp, err := g.cl.Request(g.ctx, &req)
	if err != nil {
		return err
	}
	resp := kresp.(*kmsg.JoinGroupResponse)

	if err = kerr.ErrorForCode(resp.ErrorCode); err != nil {
		switch err {
		case kerr.MemberIDRequired:
			g.memberID = resp.MemberID // KIP-394
			goto start
		case kerr.UnknownMemberID:
			g.memberID = ""
			goto start
		}
		return err // Request retries as necesary, so this must be a failure
	}

	g.memberID = resp.MemberID
	g.generation = resp.Generation

	var plan balancePlan
	if resp.LeaderID == resp.MemberID {
		plan, err = g.balanceGroup(resp.Protocol, resp.Members)
		if err != nil {
			return err
		}
		g.setLeader()
	}

	if err = g.syncGroup(plan, resp.Generation); err != nil {
		if err == kerr.RebalanceInProgress {
			goto start
		}
		return err
	}

	return nil
}

func (g *groupConsumer) syncGroup(plan balancePlan, generation int32) error {
	req := kmsg.SyncGroupRequest{
		Group:           g.id,
		Generation:      generation,
		MemberID:        g.memberID,
		InstanceID:      g.instanceID,
		GroupAssignment: plan.intoAssignment(),
	}
	kresp, err := g.cl.Request(g.ctx, &req)
	if err != nil {
		return err // Request retries as necesary, so this must be a failure
	}
	resp := kresp.(*kmsg.SyncGroupResponse)

	kassignment := new(kmsg.GroupMemberAssignment)
	if err = kassignment.ReadFrom(resp.MemberAssignment); err != nil {
		return err
	}

	// Past this point, we will fall into the setupAssigned prerevoke code,
	// meaning for cooperative, we will revoke what we need to.
	if g.cooperative {
		g.lastAssigned = g.nowAssigned
	}
	g.nowAssigned = make(map[string][]int32)
	for _, topic := range kassignment.Topics {
		g.nowAssigned[topic.Topic] = topic.Partitions
	}
	return nil
}

func (g *groupConsumer) joinGroupProtocols() []kmsg.JoinGroupRequestProtocol {
	g.mu.Lock()
	topics := make([]string, 0, len(g.using))
	for topic := range g.using {
		topics = append(topics, topic)
	}
	g.mu.Unlock()
	var protos []kmsg.JoinGroupRequestProtocol
	for _, balancer := range g.balancers {
		protos = append(protos, kmsg.JoinGroupRequestProtocol{
			Name: balancer.protocolName(),
			Metadata: balancer.metaFor(
				topics,
				g.nowAssigned,
				g.generation,
			),
		})
	}
	return protos
}

// fetchOffsets is issued once we join a group to see what the prior commits
// were for the partitions we were assigned.
func (g *groupConsumer) fetchOffsets(ctx context.Context, newAssigned map[string][]int32) error {
	req := kmsg.OffsetFetchRequest{
		Group: g.id,
	}
	for topic, partitions := range newAssigned {
		req.Topics = append(req.Topics, kmsg.OffsetFetchRequestTopic{
			Topic:      topic,
			Partitions: partitions,
		})
	}
	kresp, err := g.cl.Request(ctx, &req)
	if err != nil {
		return err
	}
	resp := kresp.(*kmsg.OffsetFetchResponse)
	errCode := resp.ErrorCode
	if resp.Version < 2 && len(resp.Topics) > 0 && len(resp.Topics[0].Partitions) > 0 {
		errCode = resp.Topics[0].Partitions[0].ErrorCode
	}
	if err = kerr.ErrorForCode(errCode); err != nil && !kerr.IsRetriable(err) {
		return err
	}

	offsets := make(map[string]map[int32]Offset)
	for _, rTopic := range resp.Topics {
		topicOffsets := make(map[int32]Offset)
		offsets[rTopic.Topic] = topicOffsets
		for _, rPartition := range rTopic.Partitions {
			if rPartition.ErrorCode != 0 {
				return kerr.ErrorForCode(rPartition.ErrorCode)
			}
			offset := Offset{
				request: rPartition.Offset,
				epoch:   -1,
			}
			if resp.Version >= 5 { // KIP-320
				offset.epoch = rPartition.LeaderEpoch
			}
			if rPartition.Offset == -1 {
				offset = g.cl.cfg.resetOffset
			}
			topicOffsets[rPartition.Partition] = offset
		}
	}

	// If we are an eager consumer, joining a group invalidates anything we
	// were consuming (in reality, we have already invalidated everything
	// we were consuming due to leaving the group to rejoin, or because
	// this is the first join and there is nothing to invalidate).
	//
	// If we are a cooperative consumer, we fetch offsets for only newly
	// assigned partitions and we must merge these new partitions in into
	// what we were consuming.
	assignHow := assignInvalidateAll
	if g.cooperative {
		assignHow = assignWithoutInvalidating
	}
	if !g.c.maybeAssignPartitions(&g.seq, offsets, assignHow) {
		return errors.New("stale group")
	}
	g.c.resetAndLoadOffsets()
	return nil
}

// findNewAssignments is called under the consumer lock at the end of a
// metadata update, updating the topics the group wants to use and other
// metadata.
//
// This joins the group if
//  - the group has never been joined
//  - new topics are found for consuming (changing this consumer's join metadata)
//
// Additionally, if the member is the leader, this rejoins the group if the
// leader notices new partitions in an existing topic. This only focuses on
// topics the leader itself owns; it can be added in the future to focus on all
// topics, which would support groups that consume disparate topics. Ideally,
// this is uncommon. This does not rejoin if the leader notices a partition is
// lost, which is finicky.
func (g *groupConsumer) findNewAssignments(topics map[string]*topicPartitions) {
	g.mu.Lock()
	defer g.mu.Unlock()

	type change struct {
		isNew bool
		delta int
	}

	var numNew int
	toChange := make(map[string]change, len(topics))
	for topic, topicPartitions := range topics {
		numPartitions := len(topicPartitions.load().partitions)
		// If we are already using this topic, add that it changed if
		// there are more partitions than we were using prior.
		if used, exists := g.using[topic]; exists {
			if numPartitions-used > 0 {
				toChange[topic] = change{delta: numPartitions - used}
			}
			continue
		}

		var useTopic bool
		if g.regexTopics {
			if _, exists := g.reSeen[topic]; !exists {
				g.reSeen[topic] = struct{}{} // set we have seen so we do not reevaluate next time
				for reTopic := range g.topics {
					if match, _ := regexp.MatchString(reTopic, topic); match {
						useTopic = true
						break
					}
				}
			}
		} else {
			_, useTopic = g.topics[topic]
		}

		if useTopic {
			if g.regexTopics && topicPartitions.load().isInternal {
				continue
			}
			toChange[topic] = change{isNew: true, delta: numPartitions}
			numNew++
		}

	}

	if len(toChange) == 0 {
		return
	}

	wasManaging := len(g.using) != 0
	for topic, change := range toChange {
		g.using[topic] += change.delta
	}

	if !wasManaging {
		go g.manage()
	}

	if numNew > 0 || g.leader {
		g.rejoin()
	}
}

// uncommit tracks the latest offset polled (+1) and the latest commit.
// The reason head is just past the latest offset is because we want
// to commit TO an offset, not BEFORE an offset.
type uncommit struct {
	head      EpochOffset
	committed EpochOffset
}

// EpochOffset combines a record offset with the leader epoch the broker
// was at when the record was written.
type EpochOffset struct {
	Epoch  int32
	Offset int64
}

type uncommitted map[string]map[int32]uncommit

// updateUncommitted sets the latest uncommitted offset. This is called under
// the consumer lock, and grabs the group lock to ensure no collision with
// commit.
func (g *groupConsumer) updateUncommitted(fetches Fetches) {
	g.mu.Lock()
	defer g.mu.Unlock()

	for _, fetch := range fetches {
		var topicOffsets map[int32]uncommit
		for _, topic := range fetch.Topics {
			for _, partition := range topic.Partitions {
				if len(partition.Records) == 0 {
					continue
				}
				final := partition.Records[len(partition.Records)-1]

				if topicOffsets == nil {
					if g.uncommitted == nil {
						g.uncommitted = make(uncommitted, 10)
					}
					topicOffsets = g.uncommitted[topic.Topic]
					if topicOffsets == nil {
						topicOffsets = make(map[int32]uncommit, 20)
						g.uncommitted[topic.Topic] = topicOffsets
					}
				}
				uncommit, exists := topicOffsets[partition.Partition]
				// Our new head points just past the final consumed offset,
				// that is, if we rejoin, this is the offset to begin at.
				newOffset := final.Offset + 1
				if exists && uncommit.head.Offset > newOffset {
					continue // odd
				}
				uncommit.head = EpochOffset{
					final.LeaderEpoch, // -1 if old message / unknown
					newOffset,
				}
				topicOffsets[partition.Partition] = uncommit
			}
		}
	}
}

// updateCommitted updates the group's uncommitted map. This function triply
// verifies that the resp matches the req as it should and that the req does
// not somehow contain more than what is in our uncommitted map.
//
// NOTE if editing this function, edit updateCommittedTxn below!
func (g *groupConsumer) updateCommitted(
	req *kmsg.OffsetCommitRequest,
	resp *kmsg.OffsetCommitResponse,
) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if req.Generation != g.generation {
		return
	}
	if g.uncommitted == nil || // just in case
		len(req.Topics) != len(resp.Topics) { // bad kafka TODO fatal error?
		return
	}

	sort.Slice(req.Topics, func(i, j int) bool {
		return req.Topics[i].Topic < req.Topics[j].Topic
	})
	sort.Slice(resp.Topics, func(i, j int) bool {
		return resp.Topics[i].Topic < resp.Topics[j].Topic
	})

	for i := range resp.Topics {
		reqTopic := &req.Topics[i]
		respTopic := &resp.Topics[i]
		topic := g.uncommitted[respTopic.Topic]
		if topic == nil || // just in case
			reqTopic.Topic != respTopic.Topic || // bad kafka
			len(reqTopic.Partitions) != len(respTopic.Partitions) { // same
			continue
		}

		sort.Slice(reqTopic.Partitions, func(i, j int) bool {
			return reqTopic.Partitions[i].Partition < reqTopic.Partitions[j].Partition
		})
		sort.Slice(respTopic.Partitions, func(i, j int) bool {
			return respTopic.Partitions[i].Partition < respTopic.Partitions[j].Partition
		})

		for i := range respTopic.Partitions {
			reqPart := &reqTopic.Partitions[i]
			respPart := &respTopic.Partitions[i]
			uncommit, exists := topic[respPart.Partition]
			if !exists || // just in case
				respPart.ErrorCode != 0 || // bad commit
				reqPart.Partition != respPart.Partition { // bad kafka
				continue
			}

			uncommit.committed = EpochOffset{
				reqPart.LeaderEpoch,
				reqPart.Offset,
			}
			topic[respPart.Partition] = uncommit
		}
	}
}

func (g *groupConsumer) loopCommit() {
	ticker := time.NewTicker(g.autocommitInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
		case <-g.ctx.Done():
			return
		}

		g.mu.Lock()
		if !g.blockAuto {
			g.commit(context.Background(), g.getUncommittedLocked(), nil)
		}
		g.mu.Unlock()
	}
}

// Uncommitted returns the latest uncommitted offsets. Uncommitted offsets are
// always updated on calls to PollFetches.
//
// If there are no uncommitted offsets, this returns nil.
//
// Note that, if manually committing, you should be careful with committing
// during group rebalances. Before rejoining the group, onRevoked is called
// with all partitions that are being lost. Once onRevoked returns, this client
// tries to rejoin the group and resets its uncommitted state for all
// partitions that were revoked. You must ensure you commit before the group's
// session timeout is reached, otherwise this client will be kicked from the
// group and the commit will fail.
func (cl *Client) Uncommitted() map[string]map[int32]EpochOffset {
	cl.consumer.mu.Lock()
	defer cl.consumer.mu.Unlock()
	if cl.consumer.typ != consumerTypeGroup {
		return nil
	}
	return cl.consumer.group.getUncommitted()
}

func (g *groupConsumer) getUncommitted() map[string]map[int32]EpochOffset {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.getUncommittedLocked()
}

func (g *groupConsumer) getUncommittedLocked() map[string]map[int32]EpochOffset {
	if g.uncommitted == nil {
		return nil
	}

	var uncommitted map[string]map[int32]EpochOffset
	for topic, partitions := range g.uncommitted {
		var topicUncommitted map[int32]EpochOffset
		for partition, uncommit := range partitions {
			if uncommit.head == uncommit.committed {
				continue
			}
			if topicUncommitted == nil {
				if uncommitted == nil {
					uncommitted = make(map[string]map[int32]EpochOffset, len(g.uncommitted))
				}
				topicUncommitted = uncommitted[topic]
				if topicUncommitted == nil {
					topicUncommitted = make(map[int32]EpochOffset, len(partitions))
					uncommitted[topic] = topicUncommitted
				}
			}
			topicUncommitted[partition] = uncommit.head
		}
	}
	return uncommitted
}

// CommitOffsets commits the given offsets for a group, calling onDone with the
// commit request and either the response or an error if the response was not
// issued. If uncommitted is empty or the client is not consuming as a group,
// onDone is called with (nil, nil, nil) and this function returns immediately.
// It is OK if onDone is nil.
//
// If autocommitting is enabled, this function blocks autocommitting until this
// function is complete and the onDone has returned.
//
// This function itself does not wait for the commit to finish; that is, by
// default, this function is an asyncronous commit. You can use onDone to make
// it sync.
//
// Note that this function ensures absolute ordering of commit requests by
// canceling prior requests and ensuring they are done before executing a new
// one. This means, for absolute control, you can use this function to
// periodically commit async and then issue a final sync commit before
// quitting. This differs from the Java async commit, which does not retry
// requests to avoid trampling on future commits.
//
// If using autocommitting, autocommitting will resume once this is complete,
// committing only if the client's internal uncommitted offsets counters are
// higher than the known last commit.
//
// It is invalid to use this function to commit offsets for a transaction.
func (cl *Client) CommitOffsets(
	ctx context.Context,
	uncommitted map[string]map[int32]EpochOffset,
	onDone func(*kmsg.OffsetCommitRequest, *kmsg.OffsetCommitResponse, error),
) {
	if onDone == nil {
		onDone = func(_ *kmsg.OffsetCommitRequest, _ *kmsg.OffsetCommitResponse, _ error) {}
	}
	cl.consumer.mu.Lock()
	defer cl.consumer.mu.Unlock()
	if cl.consumer.typ != consumerTypeGroup {
		onDone(new(kmsg.OffsetCommitRequest), new(kmsg.OffsetCommitResponse), nil)
		return
	}
	if len(uncommitted) == 0 {
		onDone(new(kmsg.OffsetCommitRequest), new(kmsg.OffsetCommitResponse), nil)
		return
	}

	g := cl.consumer.group
	g.mu.Lock()
	defer g.mu.Unlock()

	g.blockAuto = true
	unblock := func(req *kmsg.OffsetCommitRequest, resp *kmsg.OffsetCommitResponse, err error) {
		if onDone != nil {
			onDone(req, resp, err)
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		g.blockAuto = false
	}

	g.commit(ctx, uncommitted, unblock)
}

// defaultRevoke commits the last fetched offsets and waits for the commit to
// finish. This is the default onRevoked function which, when combined with the
// default autocommit, ensures we never miss committing everything.
//
// Note that the heartbeat loop invalidates all buffered, unpolled fetches
// before revoking, meaning this truly will commit all polled fetches.
func (g *groupConsumer) defaultRevoke(_ context.Context, _ map[string][]int32) {
	if !g.autocommitDisable {
		wait := make(chan struct{})
		g.cl.CommitOffsets(g.ctx, g.getUncommitted(), func(_ *kmsg.OffsetCommitRequest, _ *kmsg.OffsetCommitResponse, _ error) {
			close(wait)
		})
		<-wait
	}
}

// commit is the logic for Commit; see Commit's documentation
func (g *groupConsumer) commit(
	ctx context.Context,
	uncommitted map[string]map[int32]EpochOffset,
	onDone func(*kmsg.OffsetCommitRequest, *kmsg.OffsetCommitResponse, error),
) {
	if onDone == nil { // note we must always call onDone
		onDone = func(_ *kmsg.OffsetCommitRequest, _ *kmsg.OffsetCommitResponse, _ error) {}
	}
	if len(uncommitted) == 0 { // only empty if called thru autocommit / default revoke
		onDone(new(kmsg.OffsetCommitRequest), new(kmsg.OffsetCommitResponse), nil)
		return
	}

	if g.commitCancel != nil {
		g.commitCancel() // cancel any prior commit
	}
	priorDone := g.commitDone

	commitCtx, commitCancel := context.WithCancel(g.ctx) // enable ours to be canceled and waited for
	commitDone := make(chan struct{})

	g.commitCancel = commitCancel
	g.commitDone = commitDone

	memberID := g.memberID
	req := &kmsg.OffsetCommitRequest{
		Group:      g.id,
		Generation: g.generation,
		MemberID:   memberID,
		InstanceID: g.instanceID,
	}

	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				commitCancel()
			case <-commitCtx.Done():
			}
		}()
	}

	go func() {
		defer close(commitDone) // allow future commits to continue when we are done
		defer commitCancel()
		if priorDone != nil { // wait for any prior request to finish
			<-priorDone
		}

		for topic, partitions := range uncommitted {
			req.Topics = append(req.Topics, kmsg.OffsetCommitRequestTopic{
				Topic: topic,
			})
			reqTopic := &req.Topics[len(req.Topics)-1]
			for partition, eo := range partitions {
				reqTopic.Partitions = append(reqTopic.Partitions, kmsg.OffsetCommitRequestTopicPartition{
					Partition:   partition,
					Offset:      eo.Offset,
					LeaderEpoch: eo.Epoch, // KIP-320
					Metadata:    &memberID,
				})
			}
		}

		var kresp kmsg.Response
		var err error
		if len(req.Topics) > 0 {
			kresp, err = g.cl.Request(commitCtx, req)
		}
		if err != nil {
			onDone(req, nil, err)
			return
		}
		resp := kresp.(*kmsg.OffsetCommitResponse)
		g.updateCommitted(req, resp)
		onDone(req, resp, nil)
	}()
}

////////////////////////////////////////////////////////////////////////////////////////////
// TRANSACTIONAL COMMITTING                                                               //
// MOSTLY DUPLICATED CODE DUE TO NO GENERICS AND BECAUSE THE TYPES ARE SLIGHTLY DIFFERENT //
////////////////////////////////////////////////////////////////////////////////////////////

// CommitOffsetsForTransaction is exactly like CommitOffsets, but specifically
// for use with transactional consuming and producing.
//
// Note that, like with CommitOffsets, this cancels prior unfinished commits.
// In the unlikely event that you are committing offsets multiple times within
// a single transaction, and your second commit does not include partitions
// that were in the first commit, you need to wait for the first commit to finish
// before executing the new commit.
//
// It is invalid to use this function if the client does not have a
// transactional ID. As well, it is invalid to use this function outside of a
// transaction.
func (cl *Client) CommitOffsetsForTransaction(
	ctx context.Context,
	uncommitted map[string]map[int32]EpochOffset,
	onDone func(*kmsg.TxnOffsetCommitRequest, *kmsg.TxnOffsetCommitResponse, error),
) {
	if onDone == nil {
		onDone = func(_ *kmsg.TxnOffsetCommitRequest, _ *kmsg.TxnOffsetCommitResponse, _ error) {}
	}

	if cl.cfg.txnID == nil {
		onDone(nil, nil, ErrNotTransactional)
		return
	}

	// Before committing, ensure we are at least in a transaction. We
	// unlock the producer txnMu before committing to allow EndTransaction
	// to go through, even though that could cut off our commit.
	cl.producer.txnMu.Lock()
	if !cl.producer.inTxn {
		onDone(nil, nil, ErrNotInTransaction)
		cl.producer.txnMu.Unlock()
		return
	}
	cl.consumer.mu.Lock()
	cl.producer.txnMu.Unlock()

	defer cl.consumer.mu.Unlock()
	if cl.consumer.typ != consumerTypeGroup {
		onDone(new(kmsg.TxnOffsetCommitRequest), new(kmsg.TxnOffsetCommitResponse), nil)
		return
	}
	if len(uncommitted) == 0 {
		onDone(new(kmsg.TxnOffsetCommitRequest), new(kmsg.TxnOffsetCommitResponse), nil)
		return
	}

	g := cl.consumer.group
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.offsetsAddedToTxn {
		if err := cl.addOffsetsToTxn(g.ctx, g.id); err != nil {
			if onDone != nil {
				onDone(nil, nil, err)
			}
			return
		}
	}

	g.commitTxn(ctx, uncommitted, onDone)
}

// addOffsetsToTxn ties a transactional producer to a group. Since this
// requires a producer ID, this initializes one if it is not yet initialized.
// This would only be the case if trying to commit before the init that occurs
// on the first produce is complete.
func (c *Client) addOffsetsToTxn(ctx context.Context, group string) error {
	var idLoadingCh <-chan struct{}
	c.producer.idMu.Lock()
	if atomic.LoadUint32(&c.producer.idLoaded) == 0 {
		if c.producer.idLoadingCh == nil {
			c.producer.idLoadingCh = make(chan struct{})
			go c.initProducerID()
		}
		idLoadingCh = c.producer.idLoadingCh
	}
	c.producer.idMu.Unlock()
	if idLoadingCh != nil {
		<-idLoadingCh
	}
	if atomic.LoadUint32(&c.producer.idLoaded) == 0 {
		return errors.New("unable to init producer ID")
	}

	kresp, err := c.Request(ctx, &kmsg.AddOffsetsToTxnRequest{
		TransactionalID: *c.cfg.txnID,
		ProducerID:      c.producer.id,
		ProducerEpoch:   c.producer.epoch,
		Group:           group,
	})
	if err != nil {
		return err
	}
	resp := kresp.(*kmsg.AddOffsetsToTxnResponse)
	if err = kerr.ErrorForCode(resp.ErrorCode); err != nil {
		return err
	}
	return nil
}

// commitTxn is ALMOST EXACTLY THE SAME as commit, but changed for txn types.
// We likely do not need to try to hard to invalidate old commits, since there
// should only be one commit per transaction.
func (g *groupConsumer) commitTxn(
	ctx context.Context,
	uncommitted map[string]map[int32]EpochOffset,
	onDone func(*kmsg.TxnOffsetCommitRequest, *kmsg.TxnOffsetCommitResponse, error),
) {
	if onDone == nil { // note we must always call onDone
		onDone = func(_ *kmsg.TxnOffsetCommitRequest, _ *kmsg.TxnOffsetCommitResponse, _ error) {}
	}
	if len(uncommitted) == 0 { // only empty if called thru autocommit / default revoke
		onDone(new(kmsg.TxnOffsetCommitRequest), new(kmsg.TxnOffsetCommitResponse), nil)
		return
	}

	if g.commitCancel != nil {
		g.commitCancel() // cancel any prior commit
	}
	priorDone := g.commitDone

	commitCtx, commitCancel := context.WithCancel(g.ctx) // enable ours to be canceled and waited for
	commitDone := make(chan struct{})

	g.commitCancel = commitCancel
	g.commitDone = commitDone

	memberID := g.memberID
	req := &kmsg.TxnOffsetCommitRequest{
		TransactionalID: *g.cl.cfg.txnID,
		Group:           g.id,
		ProducerID:      g.cl.producer.id,
		ProducerEpoch:   g.cl.producer.epoch,
	}

	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				commitCancel()
			case <-commitCtx.Done():
			}
		}()
	}

	go func() {
		defer close(commitDone) // allow future commits to continue when we are done
		defer commitCancel()
		if priorDone != nil { // wait for any prior request to finish
			<-priorDone
		}

		for topic, partitions := range uncommitted {
			req.Topics = append(req.Topics, kmsg.TxnOffsetCommitRequestTopic{
				Topic: topic,
			})
			reqTopic := &req.Topics[len(req.Topics)-1]
			for partition, eo := range partitions {
				reqTopic.Partitions = append(reqTopic.Partitions, kmsg.TxnOffsetCommitRequestTopicPartition{
					Partition:   partition,
					Offset:      eo.Offset,
					LeaderEpoch: eo.Epoch,
					Metadata:    &memberID,
				})
			}
		}

		var kresp kmsg.Response
		var err error
		if len(req.Topics) > 0 {
			kresp, err = g.cl.Request(commitCtx, req)
		}
		if err != nil {
			onDone(req, nil, err)
			return
		}
		resp := kresp.(*kmsg.TxnOffsetCommitResponse)
		g.updateCommittedTxn(req, resp)
		onDone(req, resp, nil)
	}()
}

// updateCommittedTxn is EXACTLY THE SAME as updateCommitted, minus generation
// checking. It's times like this function where generics would be quite
// helpful.
func (g *groupConsumer) updateCommittedTxn(
	req *kmsg.TxnOffsetCommitRequest,
	resp *kmsg.TxnOffsetCommitResponse,
) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.uncommitted == nil || // just in case
		len(req.Topics) != len(resp.Topics) { // bad kafka TODO fatal error
		return
	}

	sort.Slice(req.Topics, func(i, j int) bool {
		return req.Topics[i].Topic < req.Topics[j].Topic
	})
	sort.Slice(resp.Topics, func(i, j int) bool {
		return resp.Topics[i].Topic < resp.Topics[j].Topic
	})

	for i := range resp.Topics {
		reqTopic := &req.Topics[i]
		respTopic := &resp.Topics[i]
		topic := g.uncommitted[respTopic.Topic]
		if topic == nil || // just in case
			reqTopic.Topic != respTopic.Topic || // bad kafka
			len(reqTopic.Partitions) != len(respTopic.Partitions) { // same
			continue
		}

		sort.Slice(reqTopic.Partitions, func(i, j int) bool {
			return reqTopic.Partitions[i].Partition < reqTopic.Partitions[j].Partition
		})
		sort.Slice(respTopic.Partitions, func(i, j int) bool {
			return respTopic.Partitions[i].Partition < respTopic.Partitions[j].Partition
		})

		for i := range respTopic.Partitions {
			reqPart := &reqTopic.Partitions[i]
			respPart := &respTopic.Partitions[i]
			uncommit, exists := topic[respPart.Partition]
			if !exists || // just in case
				respPart.ErrorCode != 0 || // bad commit
				reqPart.Partition != respPart.Partition { // bad kafka
				continue
			}

			uncommit.committed = EpochOffset{
				reqPart.LeaderEpoch,
				reqPart.Offset,
			}
			topic[respPart.Partition] = uncommit
		}
	}
}
