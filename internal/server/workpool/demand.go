package workpool

import (
	"errors"
	"math"
	"time"

	"github.com/lifei6671/xtunnel/internal/protocol/validate"
)

var (
	// ErrInvalidDemand 表示目标、Budget、Lease ID 或 TTL 无法形成合法 WorkDemand。
	ErrInvalidDemand = errors.New("server work demand is invalid")
	// ErrDemandGenerationExhausted 表示 generation 已达 uint64 上限，不能回绕复用。
	ErrDemandGenerationExhausted = errors.New("server work demand generation is exhausted")
)

type demandState struct {
	generation    uint64
	desired       uint32
	leaseID       string
	leaseDeadline time.Duration
}

// DemandRequest 是上层策略与全局 Budget Manager 交给 per-Session Pool 的裁决输入。
//
// DesiredNonActive 是绝对目标；BudgetSlots 是全局/Tunnel/Connector/FD 公平预算允许
// 本轮最多预留的槽位。只有最终确实需要新槽位时才要求 LeaseID 与 LeaseTTL 有效。
type DemandRequest struct {
	DesiredNonActive uint32
	BudgetSlots      uint32
	LeaseID          string
	LeaseTTL         time.Duration
}

// Demand 描述待放入 Control WorkDemand 的冻结字段，不直接依赖 Protobuf 或 Outbox。
type Demand struct {
	BudgetLeaseID     string
	DesiredNonActive  uint32
	MaxNewConnections uint32
	LeaseTTLMillis    uint32
	Generation        uint64
}

// BudgetLeaseGrant 描述上层需要同时发布给 WorkHello Authenticator 的 Lease。
type BudgetLeaseGrant struct {
	Session Session
	LeaseID string
	Slots   uint32
	TTL     time.Duration
}

// DemandHandoff 是一次已线性化 Demand 更新的跨组件交接结果。
//
// 上层应先 GrantLease，再 Enqueue Demand；任一步失败都应关闭当前 Session。Grant 为
// nil 表示目标降低/取消，不得调用 Authenticator.GrantLease。ReplacedLeaseID 非空时，
// 全局 Budget Manager 应归还旧 Lease 尚未消费的预留槽位。
type DemandHandoff struct {
	Session         Session
	Demand          Demand
	Grant           *BudgetLeaseGrant
	ReplacedLeaseID string
}

// DecideDemand 根据当前 Pool 计数、Session 上限与已分配预算生成合并后的 Demand。
//
// 相同绝对目标且当前 Lease 未过期时不会重复产生 handoff。目标变化、取消或 Lease
// 到期后仍有缺口时 generation 才递增。返回 emitted=false 表示本轮已被合并。
func (pool *Pool) DecideDemand(request DemandRequest) (handoff DemandHandoff, emitted bool, err error) {
	if pool == nil {
		return DemandHandoff{}, false, ErrInvalidDemand
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.closed {
		return DemandHandoff{}, false, ErrPoolClosed
	}
	now := pool.clock()
	if now < 0 {
		return DemandHandoff{}, false, ErrInvalidDemand
	}

	previousLeaseID := pool.demand.leaseID
	leaseExpired := previousLeaseID != "" && now >= pool.demand.leaseDeadline
	if leaseExpired {
		pool.demand.leaseID = ""
		pool.demand.leaseDeadline = 0
	}

	active := pool.countLocked(StateActive)
	maximumNonActive := pool.maxTotal - active
	desired := min(request.DesiredNonActive, maximumNonActive)
	nonActive := pool.nonActiveLocked()
	gap := saturatingSubtract(desired, nonActive)
	connectingRoom := saturatingSubtract(pool.maxConnecting, pool.countLocked(StateConnecting))
	totalRoom := saturatingSubtract(pool.maxTotal, pool.totalLocked())
	maxNew := min(gap, request.BudgetSlots, connectingRoom, totalRoom)

	// 未过期的同目标 Demand 已由 Agent 持有；不能因相同公网请求重复广播或重复
	// Grant Lease。目标上升但尚未获得预算时也不能先发送一个没有 Lease 的 Demand，
	// 应等全局 Budget Manager 真正给出槽位后再发布。
	leaseActive := pool.demand.leaseID != "" && !leaseExpired
	if desired == pool.demand.desired && leaseActive {
		return DemandHandoff{}, false, nil
	}
	if maxNew == 0 && desired >= pool.demand.desired {
		return DemandHandoff{}, false, nil
	}
	if maxNew > 0 {
		if validate.ValidateID(request.LeaseID, "lease_") != nil || request.LeaseID == previousLeaseID {
			return DemandHandoff{}, false, ErrInvalidDemand
		}
		leaseTTLMillis, valid := durationMillis(request.LeaseTTL)
		if !valid || now+request.LeaseTTL <= now {
			return DemandHandoff{}, false, ErrInvalidDemand
		}
		if pool.demand.generation == math.MaxUint64 {
			return DemandHandoff{}, false, ErrDemandGenerationExhausted
		}
		pool.demand.generation++
		pool.demand.desired = desired
		pool.demand.leaseID = request.LeaseID
		pool.demand.leaseDeadline = now + request.LeaseTTL
		grant := &BudgetLeaseGrant{
			Session: pool.session, LeaseID: request.LeaseID, Slots: maxNew, TTL: request.LeaseTTL,
		}
		return DemandHandoff{
			Session: pool.session,
			Demand: Demand{
				BudgetLeaseID: request.LeaseID, DesiredNonActive: desired,
				MaxNewConnections: maxNew, LeaseTTLMillis: leaseTTLMillis,
				Generation: pool.demand.generation,
			},
			Grant: grant, ReplacedLeaseID: previousLeaseID,
		}, true, nil
	}

	// 目标降低或取消时仍要发送更高 generation，使 Agent 丢弃旧绝对目标；没有
	// 新连接槽位就不生成虚假的零槽位 Authenticator Lease。
	if pool.demand.generation == math.MaxUint64 {
		return DemandHandoff{}, false, ErrDemandGenerationExhausted
	}
	pool.demand.generation++
	pool.demand.desired = desired
	pool.demand.leaseID = ""
	pool.demand.leaseDeadline = 0
	return DemandHandoff{
		Session:         pool.session,
		Demand:          Demand{DesiredNonActive: desired, Generation: pool.demand.generation},
		ReplacedLeaseID: previousLeaseID,
	}, true, nil
}

func durationMillis(duration time.Duration) (uint32, bool) {
	if duration <= 0 || duration%time.Millisecond != 0 {
		return 0, false
	}
	milliseconds := duration / time.Millisecond
	if milliseconds > math.MaxUint32 {
		return 0, false
	}
	return uint32(milliseconds), true
}

func saturatingSubtract(left, right uint32) uint32 {
	if right >= left {
		return 0
	}
	return left - right
}
