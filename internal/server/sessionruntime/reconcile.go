package sessionruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/deterministic"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/safego"
	serversnapshot "github.com/lifei6671/xtunnel/internal/server/snapshot"
)

const snapshotReconcileInterval = 5 * time.Second

type snapshotCandidate struct {
	snapshot *protocolv1.TunnelSnapshot
	revision uint64
	digest   [sha256.Size]byte
}

type snapshotSend struct {
	managed   *managedSession
	candidate *snapshotCandidate
}

type snapshotFailure struct {
	managed *managedSession
	err     error
}

// Start 启动 Manager 唯一的 Snapshot Reconcile Loop。调用方必须在开放 Gateway 前调用。
func (manager *Manager) Start(parent context.Context) error {
	if manager == nil || parent == nil {
		return ErrInvalidOptions
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.snapshotMu.Lock()
	defer manager.snapshotMu.Unlock()
	if manager.shutdownStarted {
		return ErrReconcilerNotRunning
	}
	if manager.snapshotStarted {
		return ErrReconcilerAlreadyStarted
	}

	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	manager.snapshotStarted = true
	manager.snapshotAccepting = true
	manager.snapshotCancel = cancel
	manager.snapshotDone = done
	safego.Go(func(err error) {
		manager.snapshotMu.Lock()
		manager.snapshotErr = fmt.Errorf("server snapshot reconcile loop: %w", err)
		manager.snapshotAccepting = false
		report := manager.options.ReportRuntimeError
		reported := manager.snapshotErr
		manager.snapshotMu.Unlock()
		cancel()
		if report != nil {
			report(reported)
		}
	}, func() {
		manager.snapshotMu.Lock()
		manager.snapshotAccepting = false
		manager.snapshotMu.Unlock()
		close(done)
	}, func() {
		manager.snapshotLoop(ctx)
	})
	return nil
}

// MarkDirty 合并一个 Tunnel 的最新 Desired State 唤醒；它从不等待 Build。
// 该方法直接实现 Application Service 的提交后通知契约。
func (manager *Manager) MarkDirty(tunnelID string) error {
	if manager == nil || !identity.ValidTunnelID(tunnelID) {
		return ErrInvalidOptions
	}
	manager.snapshotMu.Lock()
	err := manager.markSnapshotDirtyLocked(tunnelID)
	manager.snapshotMu.Unlock()
	return err
}

// SnapshotError 返回 Tunnel 最近一次未恢复的完整 Snapshot 构建失败。
// 成功构建并通过 generation fence 后会清除该错误；旧 Runtime 在此期间保持不变。
func (manager *Manager) SnapshotError(tunnelID string) (error, bool) {
	if manager == nil || !identity.ValidTunnelID(tunnelID) {
		return nil, false
	}
	manager.snapshotMu.Lock()
	defer manager.snapshotMu.Unlock()
	err, exists := manager.snapshotFailures[tunnelID]
	return err, exists
}

func (manager *Manager) markSnapshotDirtyLocked(tunnelID string) error {
	if !manager.snapshotStarted || !manager.snapshotAccepting {
		return ErrReconcilerNotRunning
	}
	if manager.snapshotGeneration == ^uint64(0) {
		return errors.New("server snapshot reconcile generation is exhausted")
	}
	manager.snapshotGeneration++
	manager.snapshotDirty[tunnelID] = manager.snapshotGeneration
	select {
	case manager.snapshotWake <- struct{}{}:
	default:
	}
	return nil
}

func (manager *Manager) awaitInitialSnapshot(ctx context.Context, managed *managedSession) error {
	if err := manager.MarkDirty(managed.session.TunnelID); err != nil {
		return fmt.Errorf("mark initial TunnelSnapshot dirty: %w", err)
	}
	select {
	case <-managed.initialDone:
		managed.configMu.Lock()
		err := managed.initialErr
		managed.configMu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (manager *Manager) snapshotLoop(ctx context.Context) {
	ticker := time.NewTicker(snapshotReconcileInterval)
	defer ticker.Stop()
	defer manager.failAllInitialSnapshots(ctx.Err())

	for {
		for {
			tunnelID, generation, exists := manager.takeSnapshotBuild()
			if !exists {
				break
			}
			result, err := manager.options.SnapshotProvider.Current(ctx, tunnelID)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				failure := fmt.Errorf("load current TunnelSnapshot: %w", err)
				manager.recordSnapshotFailure(tunnelID, failure)
				manager.failInitialSnapshots(tunnelID, failure)
				continue
			}
			candidate, err := newSnapshotCandidate(tunnelID, result)
			if err != nil {
				manager.recordSnapshotFailure(tunnelID, err)
				manager.failInitialSnapshots(tunnelID, err)
				manager.reportSnapshotRuntimeError(err)
				continue
			}
			stale, sends, failures := manager.commitSnapshotCandidate(tunnelID, generation, candidate)
			if stale {
				continue
			}
			manager.clearSnapshotFailure(tunnelID)
			manager.finishSnapshotCommit(sends, failures)
		}

		select {
		case <-ctx.Done():
			return
		case <-manager.snapshotWake:
		case <-ticker.C:
			manager.markCurrentTunnelsDirty()
		}
	}
}

func (manager *Manager) recordSnapshotFailure(tunnelID string, err error) {
	manager.snapshotMu.Lock()
	manager.snapshotFailures[tunnelID] = err
	manager.snapshotMu.Unlock()
}

func (manager *Manager) clearSnapshotFailure(tunnelID string) {
	manager.snapshotMu.Lock()
	delete(manager.snapshotFailures, tunnelID)
	manager.snapshotMu.Unlock()
}

func (manager *Manager) takeSnapshotBuild() (string, uint64, bool) {
	manager.snapshotMu.Lock()
	defer manager.snapshotMu.Unlock()
	if !manager.snapshotAccepting {
		return "", 0, false
	}
	for tunnelID, generation := range manager.snapshotDirty {
		delete(manager.snapshotDirty, tunnelID)
		return tunnelID, generation, true
	}
	return "", 0, false
}

func (manager *Manager) markCurrentTunnelsDirty() {
	manager.mu.Lock()
	tunnels := make(map[string]struct{}, len(manager.byConnector))
	for key := range manager.byConnector {
		tunnels[key.tunnelID] = struct{}{}
	}
	manager.mu.Unlock()
	for tunnelID := range tunnels {
		if err := manager.MarkDirty(tunnelID); err != nil && !errors.Is(err, ErrReconcilerNotRunning) {
			manager.reportSnapshotRuntimeError(err)
		}
	}
}

func newSnapshotCandidate(tunnelID string, result serversnapshot.Result) (*snapshotCandidate, error) {
	if result.Snapshot == nil || result.Snapshot.GetTunnelId() != tunnelID {
		return nil, errors.New("invalid current TunnelSnapshot identity")
	}
	encoded, err := deterministic.MarshalSnapshot(result.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("encode current TunnelSnapshot: %w", err)
	}
	return &snapshotCandidate{
		snapshot: proto.Clone(result.Snapshot).(*protocolv1.TunnelSnapshot),
		revision: result.Snapshot.GetRevision(),
		digest:   sha256.Sum256(encoded),
	}, nil
}

func (manager *Manager) commitSnapshotCandidate(
	tunnelID string,
	generation uint64,
	candidate *snapshotCandidate,
) (bool, []snapshotSend, []snapshotFailure) {
	manager.mu.Lock()
	manager.snapshotMu.Lock()
	if manager.shutdownStarted || !manager.snapshotAccepting || manager.snapshotDirty[tunnelID] > generation {
		manager.snapshotMu.Unlock()
		manager.mu.Unlock()
		return true, nil, nil
	}

	var sends []snapshotSend
	var failures []snapshotFailure
	for key, managed := range manager.byConnector {
		if key.tunnelID != tunnelID {
			continue
		}
		send, err := managed.stageSnapshot(candidate)
		if err != nil {
			failures = append(failures, snapshotFailure{managed: managed, err: err})
			continue
		}
		if send != nil {
			sends = append(sends, snapshotSend{managed: managed, candidate: send})
		}
	}
	manager.snapshotMu.Unlock()
	manager.mu.Unlock()
	return false, sends, failures
}

func (manager *Manager) finishSnapshotCommit(sends []snapshotSend, failures []snapshotFailure) {
	for _, failure := range failures {
		failure.managed.completeInitialSnapshot(failure.err)
		failure.managed.cancel()
		manager.reportSnapshotRuntimeError(failure.err)
	}
	for _, send := range sends {
		if err := manager.enqueueSnapshot(send.managed, send.candidate); err != nil {
			send.managed.completeInitialSnapshot(err)
			send.managed.cancel()
			continue
		}
		send.managed.completeInitialSnapshot(nil)
	}
}

func (manager *Manager) enqueueSnapshot(managed *managedSession, candidate *snapshotCandidate) error {
	if err := managed.owner.Enqueue(&protocolv1.ControlEnvelope{
		ProtocolVersion: managed.protocol,
		Payload: &protocolv1.ControlEnvelope_ConfigSnapshot{
			ConfigSnapshot: candidate.snapshot,
		},
	}); err != nil {
		return fmt.Errorf("enqueue TunnelSnapshot revision %d: %w", candidate.revision, err)
	}
	return nil
}

func (managed *managedSession) stageSnapshot(candidate *snapshotCandidate) (*snapshotCandidate, error) {
	managed.configMu.Lock()
	defer managed.configMu.Unlock()
	if candidate.revision < managed.initialMinimum && !managed.configReady {
		return nil, fmt.Errorf("invalid initial TunnelSnapshot: desired_revision=%d actual_revision=%d", managed.initialMinimum, candidate.revision)
	}
	if managed.outstanding != nil {
		switch {
		case candidate.revision < managed.outstanding.revision:
			return nil, nil
		case candidate.revision == managed.outstanding.revision:
			if candidate.digest != managed.outstanding.digest {
				return nil, errors.New("same Snapshot revision has different digest")
			}
			return nil, nil
		}
		if managed.pending == nil || candidate.revision > managed.pending.revision {
			managed.pending = candidate
			return nil, nil
		}
		if candidate.revision == managed.pending.revision && candidate.digest != managed.pending.digest {
			return nil, errors.New("same pending Snapshot revision has different digest")
		}
		return nil, nil
	}
	if managed.hasObserved {
		switch {
		case candidate.revision < managed.observedRevision:
			return nil, errors.New("Snapshot revision rolled back within Control Session")
		case candidate.revision == managed.observedRevision:
			if candidate.digest != managed.observedDigest {
				return nil, errors.New("observed Snapshot revision has different digest")
			}
			return nil, nil
		}
	}
	if managed.hasRejected && candidate.revision <= managed.rejectedRevision {
		return nil, nil
	}
	managed.outstanding = candidate
	return candidate, nil
}

func (managed *managedSession) completeInitialSnapshot(err error) {
	managed.initialOnce.Do(func() {
		managed.configMu.Lock()
		managed.initialErr = err
		managed.configMu.Unlock()
		close(managed.initialDone)
	})
}

func (manager *Manager) failInitialSnapshots(tunnelID string, err error) {
	manager.mu.Lock()
	managedSessions := make([]*managedSession, 0)
	for key, managed := range manager.byConnector {
		if key.tunnelID == tunnelID && !managed.isConfigReady() {
			managedSessions = append(managedSessions, managed)
		}
	}
	manager.mu.Unlock()
	for _, managed := range managedSessions {
		managed.completeInitialSnapshot(err)
		managed.cancel()
	}
}

func (manager *Manager) failAllInitialSnapshots(err error) {
	if err == nil {
		err = ErrReconcilerNotRunning
	}
	manager.mu.Lock()
	managedSessions := make([]*managedSession, 0, len(manager.byConnector))
	for _, managed := range manager.byConnector {
		if !managed.isConfigReady() {
			managedSessions = append(managedSessions, managed)
		}
	}
	manager.mu.Unlock()
	for _, managed := range managedSessions {
		managed.completeInitialSnapshot(err)
		managed.cancel()
	}
}

func (manager *Manager) reportSnapshotRuntimeError(err error) {
	if err != nil && manager.options.ReportRuntimeError != nil {
		manager.options.ReportRuntimeError(err)
	}
}
