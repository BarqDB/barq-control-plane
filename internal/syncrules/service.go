package syncrules

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/barqdb/barq-server/internal/control"
	"github.com/barqdb/barq-server/internal/dataplane"
)

const (
	revisionCollection = "sync_rule_revisions"
	headCollection     = "sync_rule_heads"
	pendingCollection  = "sync_rule_pending"
)

type Revision struct {
	Scope     dataplane.Scope     `json:"scope"`
	Revision  uint64              `json:"revision"`
	Hash      string              `json:"hash"`
	Rules     []dataplane.FLXRule `json:"rules"`
	Actor     string              `json:"actor,omitempty"`
	RequestID string              `json:"request_id,omitempty"`
	Source    string              `json:"source,omitempty"`
	CreatedAt time.Time           `json:"created_at"`
}

type Head struct {
	Scope     dataplane.Scope `json:"scope"`
	Revision  uint64          `json:"revision"`
	Hash      string          `json:"hash"`
	UpdatedAt time.Time       `json:"updated_at"`
}

type ApplyInput struct {
	ExpectedRevision uint64              `json:"expected_revision"`
	Rules            []dataplane.FLXRule `json:"rules"`
	RequestID        string              `json:"-"`
	Actor            string              `json:"-"`
}

type pendingApply struct {
	Scope            dataplane.Scope     `json:"scope"`
	ExpectedRevision uint64              `json:"expected_revision"`
	TargetRevision   uint64              `json:"target_revision"`
	Rules            []dataplane.FLXRule `json:"rules"`
	Actor            string              `json:"actor,omitempty"`
	RequestID        string              `json:"request_id"`
	CreatedAt        time.Time           `json:"created_at"`
}

type Service struct {
	data  dataplane.DataPlane
	store control.Store
	now   func() time.Time
}

func New(data dataplane.DataPlane, store control.Store) *Service {
	return &Service{data: data, store: store, now: time.Now}
}

// Reconcile resumes rule applies that were interrupted between Core and control.barq.
func (s *Service) Reconcile(ctx context.Context) error {
	pendingRecords, err := s.store.List(ctx, pendingCollection, "")
	if err != nil {
		return err
	}
	for _, record := range pendingRecords {
		pending, err := control.Decode[pendingApply](record)
		if err != nil {
			return err
		}
		current, err := s.data.ReadFLXRules(ctx, dataplane.FLXRulesReadRequest{RequestID: pending.RequestID, Scope: pending.Scope})
		if err != nil {
			return err
		}
		if current.Revision == pending.ExpectedRevision {
			result, err := s.data.ApplyFLXRules(ctx, dataplane.FLXRulesChangeRequest{
				RequestID: pending.RequestID, Scope: pending.Scope, ExpectedRevision: pending.ExpectedRevision,
				TargetRevision: pending.TargetRevision, Rules: pending.Rules,
			})
			if err != nil {
				return err
			}
			current = result.FLXRuleSet
		}
		if current.Revision >= pending.TargetRevision &&
			(current.Revision != pending.TargetRevision || !sameRules(current.Rules, pending.Rules)) {
			// Core moved past this request, or used this revision for another
			// rule set. Replaying it can never be correct.
			if err := s.store.Delete(ctx, pendingCollection, record.Key, &record.Version); err != nil &&
				!dataplane.IsCode(err, dataplane.CodeNotFound) {
				return err
			}
			continue
		}
		if current.Revision != pending.TargetRevision || !sameRules(current.Rules, pending.Rules) {
			return &dataplane.Error{Code: dataplane.CodeConflict, Message: "interrupted sync-rule apply no longer matches Core"}
		}
		revision := Revision{
			Scope: pending.Scope, Revision: current.Revision, Hash: current.Hash,
			Rules: append([]dataplane.FLXRule(nil), current.Rules...), Actor: pending.Actor,
			RequestID: pending.RequestID, Source: "recovered", CreatedAt: pending.CreatedAt,
		}
		if err := s.finishApply(ctx, revision, record.Key, record.Version); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) Schema(ctx context.Context, scope dataplane.Scope, requestID string) (dataplane.Schema, error) {
	return s.data.ReadSchema(ctx, dataplane.SchemaReadRequest{RequestID: requestID, Scope: scope})
}

func (s *Service) Current(ctx context.Context, scope dataplane.Scope, requestID string) (dataplane.FLXRuleSet, error) {
	current, err := s.data.ReadFLXRules(ctx, dataplane.FLXRulesReadRequest{RequestID: requestID, Scope: scope})
	if err != nil {
		return dataplane.FLXRuleSet{}, err
	}
	if current.Revision > 0 {
		if err := s.reconcile(ctx, scope, current, requestID); err != nil {
			return dataplane.FLXRuleSet{}, err
		}
	}
	return current, nil
}

func (s *Service) Plan(ctx context.Context, scope dataplane.Scope, input ApplyInput) (dataplane.FLXRulesResult, error) {
	return s.data.PlanFLXRules(ctx, dataplane.FLXRulesChangeRequest{
		RequestID: input.RequestID, Scope: scope, ExpectedRevision: input.ExpectedRevision,
		TargetRevision: input.ExpectedRevision + 1, Rules: input.Rules,
	})
}

func (s *Service) Apply(ctx context.Context, scope dataplane.Scope, input ApplyInput) (dataplane.FLXRulesResult, error) {
	if input.RequestID == "" {
		return dataplane.FLXRulesResult{}, invalid("request ID is required")
	}
	target := input.ExpectedRevision + 1
	pending := pendingApply{
		Scope: scope, ExpectedRevision: input.ExpectedRevision, TargetRevision: target,
		Rules: append([]dataplane.FLXRule(nil), input.Rules...), Actor: input.Actor,
		RequestID: input.RequestID, CreatedAt: s.now().UTC(),
	}
	pendingKey := scopeKey(scope) + "/" + input.RequestID
	pendingJSON, err := control.Encode(pending)
	if err != nil {
		return dataplane.FLXRulesResult{}, err
	}
	zero := uint64(0)
	pendingRecord, err := s.store.Put(ctx, pendingCollection, pendingKey, pendingJSON, &zero)
	if err != nil {
		if !dataplane.IsCode(err, dataplane.CodeConflict) {
			return dataplane.FLXRulesResult{}, err
		}
		pendingRecord, err = s.store.Get(ctx, pendingCollection, pendingKey)
		if err != nil {
			return dataplane.FLXRulesResult{}, err
		}
		stored, decodeErr := control.Decode[pendingApply](pendingRecord)
		if decodeErr != nil || !samePending(stored, pending) {
			return dataplane.FLXRulesResult{}, &dataplane.Error{Code: dataplane.CodeConflict, Message: "request ID was already used for another sync-rule apply"}
		}
	}

	result, err := s.data.ApplyFLXRules(ctx, dataplane.FLXRulesChangeRequest{
		RequestID: input.RequestID, Scope: scope, ExpectedRevision: input.ExpectedRevision,
		TargetRevision: target, Rules: input.Rules,
	})
	if err != nil {
		// A rejected request did not change Core. Do not leave it in the
		// recovery queue, where it would block the next server start.
		if isFinalApplyError(err) {
			_ = s.store.Delete(ctx, pendingCollection, pendingKey, &pendingRecord.Version)
		}
		return dataplane.FLXRulesResult{}, err
	}
	revision := Revision{
		Scope: scope, Revision: result.Revision, Hash: result.Hash,
		Rules: append([]dataplane.FLXRule(nil), result.Rules...), Actor: input.Actor,
		RequestID: input.RequestID, Source: "apply", CreatedAt: s.now().UTC(),
	}
	if err := s.finishApply(ctx, revision, pendingKey, pendingRecord.Version); err != nil {
		return dataplane.FLXRulesResult{}, err
	}
	return result, nil
}

func (s *Service) Test(ctx context.Context, scope dataplane.Scope, input dataplane.FLXRulesTestRequest) (dataplane.FLXRulesTestResult, error) {
	input.Scope = scope
	return s.data.TestFLXRules(ctx, input)
}

func (s *Service) History(ctx context.Context, scope dataplane.Scope) ([]Revision, error) {
	records, err := s.store.List(ctx, revisionCollection, scopeKey(scope)+"/")
	if err != nil {
		return nil, err
	}
	revisions := make([]Revision, 0, len(records))
	for _, record := range records {
		revision, err := control.Decode[Revision](record)
		if err != nil {
			return nil, err
		}
		revisions = append(revisions, revision)
	}
	sort.Slice(revisions, func(i, j int) bool { return revisions[i].Revision > revisions[j].Revision })
	return revisions, nil
}

func (s *Service) Restore(ctx context.Context, scope dataplane.Scope, revision uint64, input ApplyInput) (dataplane.FLXRulesResult, error) {
	record, err := s.store.Get(ctx, revisionCollection, revisionKey(scope, revision))
	if err != nil {
		return dataplane.FLXRulesResult{}, err
	}
	stored, err := control.Decode[Revision](record)
	if err != nil {
		return dataplane.FLXRulesResult{}, err
	}
	input.Rules = append([]dataplane.FLXRule(nil), stored.Rules...)
	return s.Apply(ctx, scope, input)
}

func (s *Service) reconcile(ctx context.Context, scope dataplane.Scope, current dataplane.FLXRuleSet, requestID string) error {
	revisionKey := revisionKey(scope, current.Revision)
	if record, err := s.store.Get(ctx, revisionCollection, revisionKey); err == nil {
		stored, decodeErr := control.Decode[Revision](record)
		if decodeErr != nil {
			return decodeErr
		}
		if stored.Hash != current.Hash {
			return &dataplane.Error{Code: dataplane.CodeConflict, Message: "Core sync-rule revision does not match control history"}
		}
		return s.writeHead(ctx, scope, current.Revision, current.Hash)
	} else if !dataplane.IsCode(err, dataplane.CodeNotFound) {
		return err
	}
	revision := Revision{
		Scope: scope, Revision: current.Revision, Hash: current.Hash,
		Rules: append([]dataplane.FLXRule(nil), current.Rules...), RequestID: requestID,
		Source: "recovered", CreatedAt: s.now().UTC(),
	}
	encoded, err := control.Encode(revision)
	if err != nil {
		return err
	}
	zero := uint64(0)
	if _, err := s.store.Put(ctx, revisionCollection, revisionKey, encoded, &zero); err != nil && !dataplane.IsCode(err, dataplane.CodeConflict) {
		return err
	}
	return s.writeHead(ctx, scope, current.Revision, current.Hash)
}

func (s *Service) finishApply(ctx context.Context, revision Revision, pendingKey string, pendingVersion uint64) error {
	key := revisionKey(revision.Scope, revision.Revision)
	if record, err := s.store.Get(ctx, revisionCollection, key); err == nil {
		stored, decodeErr := control.Decode[Revision](record)
		if decodeErr != nil {
			return decodeErr
		}
		if stored.Hash != revision.Hash {
			return &dataplane.Error{Code: dataplane.CodeConflict, Message: "sync-rule revision already has another hash"}
		}
		_ = s.store.Delete(ctx, pendingCollection, pendingKey, &pendingVersion)
		return s.writeHead(ctx, revision.Scope, revision.Revision, revision.Hash)
	} else if !dataplane.IsCode(err, dataplane.CodeNotFound) {
		return err
	}
	revisionJSON, err := control.Encode(revision)
	if err != nil {
		return err
	}
	head := Head{Scope: revision.Scope, Revision: revision.Revision, Hash: revision.Hash, UpdatedAt: s.now().UTC()}
	headJSON, err := control.Encode(head)
	if err != nil {
		return err
	}
	headVersion := uint64(0)
	writeHead := true
	if record, getErr := s.store.Get(ctx, headCollection, scopeKey(revision.Scope)); getErr == nil {
		stored, decodeErr := control.Decode[Head](record)
		if decodeErr != nil {
			return decodeErr
		}
		if stored.Revision > revision.Revision || (stored.Revision == revision.Revision && stored.Hash == revision.Hash) {
			writeHead = false
		} else if stored.Revision == revision.Revision {
			return &dataplane.Error{Code: dataplane.CodeConflict, Message: "sync-rule head has another hash"}
		}
		headVersion = record.Version
	} else if !dataplane.IsCode(getErr, dataplane.CodeNotFound) {
		return getErr
	}
	mutations := []control.Mutation{
		{Op: control.MutationPut, Collection: revisionCollection, Key: key, Value: revisionJSON, ExpectedVersion: uint64Pointer(0)},
		{Op: control.MutationDelete, Collection: pendingCollection, Key: pendingKey, ExpectedVersion: &pendingVersion},
	}
	if writeHead {
		mutations = append(mutations, control.Mutation{Op: control.MutationPut, Collection: headCollection, Key: scopeKey(revision.Scope), Value: headJSON, ExpectedVersion: &headVersion})
	}
	_, err = s.store.Apply(ctx, mutations)
	return err
}

func (s *Service) writeHead(ctx context.Context, scope dataplane.Scope, revision uint64, hash string) error {
	key := scopeKey(scope)
	head := Head{Scope: scope, Revision: revision, Hash: hash, UpdatedAt: s.now().UTC()}
	encoded, err := control.Encode(head)
	if err != nil {
		return err
	}
	var expected uint64
	if record, getErr := s.store.Get(ctx, headCollection, key); getErr == nil {
		stored, decodeErr := control.Decode[Head](record)
		if decodeErr != nil {
			return decodeErr
		}
		if stored.Revision == revision && stored.Hash == hash {
			return nil
		}
		if stored.Revision > revision {
			return nil
		}
		if stored.Revision == revision {
			return &dataplane.Error{Code: dataplane.CodeConflict, Message: "sync-rule head has another hash"}
		}
		expected = record.Version
	} else if !dataplane.IsCode(getErr, dataplane.CodeNotFound) {
		return getErr
	}
	_, err = s.store.Put(ctx, headCollection, key, encoded, &expected)
	return err
}

func samePending(left, right pendingApply) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) == string(rightJSON)
}

func sameRules(left, right []dataplane.FLXRule) bool {
	if len(left) != len(right) {
		return false
	}
	left = append([]dataplane.FLXRule(nil), left...)
	right = append([]dataplane.FLXRule(nil), right...)
	sort.Slice(left, func(i, j int) bool { return left[i].ObjectType < left[j].ObjectType })
	sort.Slice(right, func(i, j int) bool { return right[i].ObjectType < right[j].ObjectType })
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) == string(rightJSON)
}

func isFinalApplyError(err error) bool {
	return dataplane.IsCode(err, dataplane.CodeInvalid) ||
		dataplane.IsCode(err, dataplane.CodeNotFound) ||
		dataplane.IsCode(err, dataplane.CodeConflict) ||
		dataplane.IsCode(err, dataplane.CodePrecondition) ||
		dataplane.IsCode(err, dataplane.CodeUnauthorized) ||
		dataplane.IsCode(err, dataplane.CodeForbidden) ||
		dataplane.IsCode(err, dataplane.CodeResourceExceeded)
}

func scopeKey(scope dataplane.Scope) string {
	return fmt.Sprintf("%d:%s/%d:%s", len(scope.Tenant), scope.Tenant, len(scope.Database), scope.Database)
}

func revisionKey(scope dataplane.Scope, revision uint64) string {
	return fmt.Sprintf("%s/%020d", scopeKey(scope), revision)
}

func uint64Pointer(value uint64) *uint64 { return &value }

func invalid(message string) error {
	return &dataplane.Error{Code: dataplane.CodeInvalid, Message: message}
}
