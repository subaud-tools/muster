package dynamostore

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/subaud-tools/muster/internal/clock"
	"github.com/subaud-tools/muster/internal/store"
)

const (
	// eventsPartition is gsi2's journal partition, in event-id order. It is
	// disjoint from threadsPartition and entriesPartition: only event items
	// carry it.
	//
	// The journal is NOT indexed by actor. The brief for this backend proposed
	// a per-agent gsi1 partition, but EventQuery.Agent is a CONCERN filter, not
	// an actor filter — it matches the actor, an 'agent:<alias>' target, a bare
	// alias target (a nudge), OR any event on a thread that concerns the alias
	// (see eventConcerns). Three of those four arms cannot be served by a
	// partition keyed on the actor, and the thread arm would need its own index
	// plus a union-and-dedupe across three queries. One id-ordered partition
	// filtered in memory answers all four, and it is the same read shape
	// concerns() already accepts for the THREADS partition.
	eventsPartition = "EVENTS"

	// maxEventLimit bounds any single Events query. Mirrors the SQLite
	// backend's maxEventLimit, which is unexported.
	maxEventLimit = 1000

	// EventRetentionEnv tunes how long an event item survives before
	// DynamoDB's native TTL reaps it — the operator knob behind the `ttl`
	// attribute. Any Go duration ("720h", "168h"); it must be positive.
	EventRetentionEnv = "MUSTER_DDB_EVENT_RETENTION"

	// defaultEventRetention is 30 days, the retention the bus assumes when the
	// knob is unset.
	defaultEventRetention = 30 * 24 * time.Hour
)

// eventRetention reads the retention knob. An unparseable or non-positive
// value is an ERROR rather than a silent fallback: a typo in a deployment's
// environment would otherwise look exactly like the default, and a
// non-positive window would stamp every event with an already-expired ttl and
// reap the journal as fast as it is written.
func eventRetention() (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(EventRetentionEnv))
	if v == "" {
		return defaultEventRetention, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("dynamostore: %s=%q: %w", EventRetentionEnv, v, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("dynamostore: %s=%q must be positive", EventRetentionEnv, v)
	}
	return d, nil
}

// clampEventLimit enforces the SQLite backend's limit rule: <=0 or anything
// over the cap becomes maxEventLimit. The "backlog with Limit<=0 returns no
// rows" case is handled by Events before this is reached, exactly as in SQL.
func clampEventLimit(limit int) int {
	if limit <= 0 || limit > maxEventLimit {
		return maxEventLimit
	}
	return limit
}

func eventKey(id int64) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"pk": attrS(pkEvent(id)),
		"sk": attrN(metaSK),
	}
}

// expireAt converts a wall-clock millisecond stamp plus a retention window
// into a DynamoDB `ttl` value. DynamoDB requires epoch SECONDS: handing it
// milliseconds puts every expiry ~50,000 years out and nothing is ever reaped,
// which fails silently — the table just grows.
func expireAt(nowMillis int64, window time.Duration) int64 {
	return nowMillis/1000 + int64(window/time.Second)
}

// AppendEvent records one observability event, stamped now. Callers treat
// event logging as best-effort: an append failure must never fail the bus
// operation it describes.
//
// Every item carries a `ttl` — that attribute, enabled on the table by
// EnsureTable, is what replaces PruneEvents on this backend.
func (s *Store) AppendEvent(e store.Event) error {
	ctx := backgroundCtx()
	id, err := s.nextID(ctx, "event")
	if err != nil {
		return err
	}
	now := clock.NowMillis()
	_, err = s.c.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item: map[string]types.AttributeValue{
			"pk":        attrS(pkEvent(id)),
			"sk":        attrN(metaSK),
			"id":        attrN(id),
			"ts":        attrN(now),
			"kind":      attrS(e.Kind),
			"agent":     attrS(e.Agent),
			"target":    attrS(e.Target),
			"thread_id": attrN(e.ThreadID),
			"count":     attrN(int64(e.Count)),
			"detail":    attrS(e.Detail),
			"ttl":       attrN(expireAt(now, s.eventTTL)),
			"gsi2pk":    attrS(eventsPartition),
			"gsi2sk":    attrN(id),
		},
	})
	if err != nil {
		return fmt.Errorf("dynamostore: append event: %w", err)
	}
	return nil
}

// itemToEvent reads a journal item. Subject and Intent are deliberately left
// zero — they are joined from the event's thread by annotateEventThreads,
// never stored on the row, matching the SQLite LEFT JOIN.
func itemToEvent(item map[string]types.AttributeValue) store.Event {
	return store.Event{
		ID:       numAttr(item, "id"),
		TS:       numAttr(item, "ts"),
		Kind:     strAttr(item, "kind"),
		Agent:    strAttr(item, "agent"),
		Target:   strAttr(item, "target"),
		ThreadID: numAttr(item, "thread_id"),
		Count:    int(numAttr(item, "count")),
		Detail:   strAttr(item, "detail"),
	}
}

// eventConcerns is the Go translation of the SQLite agent-filter predicate in
// Events — "is this alias CONCERNED in this event". Four arms, and each one is
// load-bearing:
//
//	actor            the alias did it
//	'agent:<alias>'  the event was addressed to it
//	bare alias       a nudge, whose target is the bare alias
//	thread concern   any event on a thread that concerns the alias — a reply
//	                 row carries an empty target, so this is the ONLY arm that
//	                 can match the originator of the thread being replied to
//
// concerning is the set of thread ids that satisfy store's threadConcerns for
// this alias, computed once per query from the canonical concerns() helper, so
// the events filter and Inbox cannot drift apart about what "concerns me"
// means.
func eventConcerns(e store.Event, alias string, concerning map[int64]bool) bool {
	switch {
	case e.Agent == alias, e.Target == "agent:"+alias, e.Target == alias:
		return true
	case e.ThreadID > 0 && concerning[e.ThreadID]:
		return true
	default:
		return false
	}
}

// Events runs q against the journal (see store.EventQuery for mode semantics),
// annotating each row with its thread's subject and EFFECTIVE intent.
//
// One query over gsi2's EVENTS partition, walked in the query's own direction —
// descending for backlog (newest first), ascending from AfterID for follow — so
// the row cap is applied to MATCHING events in the same order the SQL's
// ORDER BY ... LIMIT applies it, not to a pre-filter page.
//
// The filter is split by what DynamoDB can evaluate server-side. Kind and
// ThreadID are plain equality on stored attributes, so they go in a
// FilterExpression and never cross the wire. The Agent arm cannot: three of its
// four arms are string shapes over `target`, and the fourth needs the alias's
// concerning-thread set, which is itself the result of several queries. That
// arm runs in memory (eventConcerns) over the same canonical concerns() set
// Inbox uses.
//
// # A follow poll can permanently SKIP an event
//
// The query reads gsi2, which is eventually consistent and carries no
// cross-item ordering guarantee — a global secondary index cannot be read
// strongly consistent at any price, so this is not a knob that was left
// unturned. If events 6 and 7 are written in that order but 7 replicates into
// the index first, a follow poll with AfterID=5 returns 7 and not 6. The
// caller advances its watermark to 7, the follow bound is strict (gsi2sk >
// :after), and 6 is never returned again. Not delayed — lost.
//
// This is the same defect class MaxEventID deliberately avoids by reading the
// journal instead of the counter, reintroduced one level down through the
// query itself. It is accepted rather than fixed: the journal is
// observability only — station's live feed — the cost is a missing line in a
// feed, and every repair costs durable in-flight state plus its own
// crash-recovery story for a surface where AppendEvent is already best-effort.
// A reader who needs completeness should re-read as a BACKLOG query, which is
// bounded by newest-first order rather than by a watermark it has already
// advanced past.
//
// It is a REAL DIVERGENCE from the SQLite backend, not a limitation the two
// share. There ids come from AUTOINCREMENT inside the inserting transaction,
// over the single connection store.Open pins, so a follow read cannot see a
// gap it will then skip past. A conformance suite must NOT assert that
// successive follow polls return every event that was written.
//
// What makes the acceptance defensible is exactly what the journal is: a feed.
// DevicePoll meets the identical hazard over the entry log and cannot accept it,
// because its watermark is the ONLY wake path for cross-device mail — a skip
// there is a badge that never lights, not a line missing from a feed. See
// pollWatermark for the contiguity-plus-overlap rule that costs; it is also what
// this path would need if anything ever hung a wake off the journal.
func (s *Store) Events(q store.EventQuery) ([]store.Event, error) {
	if q.AfterID < 0 || q.ThreadID < 0 {
		return nil, fmt.Errorf("negative id in event query")
	}
	if q.Backlog && q.Limit <= 0 {
		return nil, nil
	}
	limit := clampEventLimit(q.Limit)
	ctx := backgroundCtx()

	// The concerning-thread set is resolved once per query rather than per
	// event: it costs the same fan-out Inbox pays, and doing it per row would
	// multiply that by the page size.
	var concerning map[int64]bool
	if q.Agent != "" {
		agent, _, err := s.agentByAlias(ctx, q.Agent)
		if err != nil {
			return nil, err
		}
		// An unregistered alias has no role and no project, exactly as the
		// SQL's role and project subqueries yield NULL for one.
		agent.Alias = q.Agent
		threads, _, err := s.concerns(ctx, agent)
		if err != nil {
			return nil, err
		}
		concerning = idSet(threads)
	}

	in := &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		IndexName:              aws.String(gsi2Name),
		KeyConditionExpression: aws.String("gsi2pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": attrS(eventsPartition),
		},
		ScanIndexForward: aws.Bool(!q.Backlog),
	}
	if !q.Backlog {
		in.KeyConditionExpression = aws.String("gsi2pk = :pk AND gsi2sk > :after")
		in.ExpressionAttributeValues[":after"] = attrN(q.AfterID)
	}

	var filters []string
	names := make(map[string]string, 2)
	if q.Kind != "" {
		// Aliased because setExpr's rule applies to every expression in this
		// package: `kind` and `count` are both DynamoDB reserved words.
		filters = append(filters, "#kind = :kind")
		names["#kind"] = "kind"
		in.ExpressionAttributeValues[":kind"] = attrS(q.Kind)
	}
	if q.ThreadID > 0 {
		filters = append(filters, "#thread_id = :thread_id")
		names["#thread_id"] = "thread_id"
		in.ExpressionAttributeValues[":thread_id"] = attrN(q.ThreadID)
	}
	if len(filters) > 0 {
		in.FilterExpression = aws.String(strings.Join(filters, " AND "))
		in.ExpressionAttributeNames = names
	}
	// A page Limit is only safe to push down when nothing filters: DynamoDB
	// applies Limit to items EXAMINED, before any FilterExpression and long
	// before eventConcerns, so a limited page of a filtered query would just
	// force more round trips. Unfiltered is station's common poll, and there
	// the bound is exact.
	if q.Agent == "" && len(filters) == 0 {
		in.Limit = aws.Int32(int32(limit)) //nolint:gosec // clampEventLimit bounds limit to maxEventLimit
	}

	out, err := s.eventPage(ctx, in, q.Agent, concerning, limit)
	if err != nil {
		return nil, err
	}
	if err := s.annotateEventThreads(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// eventPage walks the journal query, keeping events that pass the in-memory
// agent filter and stopping the moment limit of them have been collected.
// Pagination is explicit rather than via queryAll because stopping early is
// the point: DynamoDB pages at 1MB regardless of how few items match, and a
// ten-row backlog must not drain the whole journal.
func (s *Store) eventPage(ctx context.Context, in *dynamodb.QueryInput,
	alias string, concerning map[int64]bool, limit int,
) ([]store.Event, error) {
	var out []store.Event
	var start map[string]types.AttributeValue
	for {
		in.ExclusiveStartKey = start
		page, err := s.c.Query(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("dynamostore: query events: %w", err)
		}
		for _, item := range page.Items {
			e := itemToEvent(item)
			if alias != "" && !eventConcerns(e, alias, concerning) {
				continue
			}
			out = append(out, e)
			if len(out) == limit {
				return out, nil
			}
		}
		if len(page.LastEvaluatedKey) == 0 {
			return out, nil
		}
		start = page.LastEvaluatedKey
	}
}

// annotateEventThreads fills Subject and Intent from each event's thread — the
// DynamoDB form of the SQLite LEFT JOIN, and LEFT is the operative word: an
// event naming a thread that no longer exists keeps both fields empty and is
// still returned.
//
// It batches the distinct thread ids of the page rather than reading the whole
// THREADS partition, because the page is already bounded by the query's limit
// while the partition is not.
func (s *Store) annotateEventThreads(ctx context.Context, evs []store.Event) error {
	ids := make([]int64, 0, len(evs))
	seen := make(map[int64]bool, len(evs))
	for _, e := range evs {
		if e.ThreadID > 0 && !seen[e.ThreadID] {
			seen[e.ThreadID] = true
			ids = append(ids, e.ThreadID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	metas, err := s.threadMetas(ctx, ids)
	if err != nil {
		return err
	}
	for i := range evs {
		item, ok := metas[evs[i].ThreadID]
		if !ok {
			continue
		}
		evs[i].Subject = strAttr(item, "subject")
		evs[i].Intent = effectiveIntent(strAttr(item, "kind"), strAttr(item, "intent"))
		evs[i].ToKind = strAttr(item, "to_kind")
		evs[i].Origin = strAttr(item, "from_agent")
		evs[i].Wake = boolAttr(item, "wake")
	}
	return nil
}

// batchGetChunk is DynamoDB's hard cap on keys per BatchGetItem request.
const batchGetChunk = 100

// threadMetas reads the metadata items for the given DISTINCT thread ids.
// Missing ids are simply absent from the result — the caller's LEFT JOIN
// semantics. Duplicate keys in one request are a ValidationException, which is
// why the caller de-duplicates rather than this doing it defensively.
//
// UnprocessedKeys is not an error: DynamoDB returns it when a request exceeds
// its throughput allowance, and dropping it would silently lose annotations.
func (s *Store) threadMetas(ctx context.Context, ids []int64) (map[int64]map[string]types.AttributeValue, error) {
	out := make(map[int64]map[string]types.AttributeValue, len(ids))
	for start := 0; start < len(ids); start += batchGetChunk {
		end := min(start+batchGetChunk, len(ids))
		keys := make([]map[string]types.AttributeValue, 0, end-start)
		for _, id := range ids[start:end] {
			keys = append(keys, threadKey(id))
		}
		for len(keys) > 0 {
			resp, err := s.c.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{
				RequestItems: map[string]types.KeysAndAttributes{
					s.table: {Keys: keys},
				},
			})
			if err != nil {
				return nil, fmt.Errorf("dynamostore: batch get threads: %w", err)
			}
			for _, item := range resp.Responses[s.table] {
				out[numAttr(item, "id")] = item
			}
			keys = resp.UnprocessedKeys[s.table].Keys
		}
	}
	return out, nil
}

// MaxEventID returns the journal high-water mark (0 on an empty journal) — the
// newest WRITTEN event id, read as one descending Limit-1 query over the
// journal partition.
//
// It deliberately does NOT read the event counter, even though that is a
// cheaper strongly-consistent single-item read. nextID hands out an id in its
// own round trip before the item is written, so the counter can already name an
// event that has not committed. Callers use this value as a follow watermark
// (list_events returns it and the next poll passes it back as AfterID), and a
// watermark over an unwritten id skips that event permanently — the same defect
// class as the MarkRead overshoot documented in the package comment.
//
// The index read is eventually consistent, and that is NOT free here — the
// hazard is SKIPPING, not lagging. Lagging is the safe direction (a caller
// that reads a smaller max simply re-asks and sees the event next poll); what
// is unsafe is returning an id with an unwritten one BELOW it, because the
// consumers use this value as their cursor verbatim (humancli/tail.go's
// follow loop and station/model.go both take max_id and pass it back as
// AfterID), so a skipped event is never emitted to them at all. Two things
// produce exactly that: allocate-before-commit, which this is the THIRD
// instance of on the DynamoDB backend (MarkRead's overshoot and DevicePoll's
// watermark are the others, see the package comment), and gsi2 replication,
// which can make a higher id visible before a lower one. So this can and does
// name an id whose predecessor no reader has seen.
//
// That is knowingly accepted rather than repaired: the journal is a feed, and
// a missing line is the whole cost (see Events). If it is ever repaired, the
// fix is not a bigger read but pollWatermark's rule from Task 14 — refuse to
// pass an id whose predecessors have not been seen, with an overlap floor so a
// never-committed id cannot stall the cursor for ever — and that rule applies
// here unchanged.
func (s *Store) MaxEventID() (int64, error) {
	out, err := s.c.Query(backgroundCtx(), &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		IndexName:              aws.String(gsi2Name),
		KeyConditionExpression: aws.String("gsi2pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": attrS(eventsPartition),
		},
		ScanIndexForward: aws.Bool(false),
		Limit:            aws.Int32(1),
	})
	if err != nil {
		return 0, fmt.Errorf("dynamostore: max event id: %w", err)
	}
	if len(out.Items) == 0 {
		return 0, nil
	}
	return numAttr(out.Items[0], "id"), nil
}

// PruneEvents is a deliberate no-op on this backend and always returns (0, nil).
//
// It is not unimplemented and it is not a stub. Event expiry here is DynamoDB's
// NATIVE TTL: EnsureTable enables TTL on the `ttl` attribute — and verifies it
// on a table it did not create, so this no-op's precondition is checked at
// Open rather than assumed — AppendEvent
// stamps every item with now + the EventRetentionEnv window, and the service
// deletes expired items itself at no write cost. Issuing deletes from here
// would duplicate that at full write-capacity price, and a cutoff-driven scan
// of the whole journal is precisely the access pattern this backend's schema
// exists to avoid.
//
// The method stays on store.API because the SQLite backend genuinely needs it —
// SQLite has no expiry mechanism, so `muster gc` is the only thing that bounds
// its journal. Removing it from the interface to delete this function would
// break the backend that uses it.
//
// The return value is honest about what happened: zero rows were pruned BY THIS
// CALL. The daemon's prune_events op reports that count to the operator, and
// "pruned: 0" against a hosted bus is the truth — the journal is bounded, just
// not by them.
func (s *Store) PruneEvents(olderThanMillis int64) (int64, error) {
	_ = olderThanMillis // the cutoff has no meaning here; retention is the ttl window
	return 0, nil
}
