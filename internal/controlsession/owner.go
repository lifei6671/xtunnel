package controlsession

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
)

var (
	// ErrInvalidOwnerOptions 表示 Owner 缺少已认证状态、连接或正数容量/超时。
	ErrInvalidOwnerOptions = errors.New("control session owner options are invalid")
	// ErrOwnerAlreadyStarted 表示同一个 Owner 被重复启动。
	ErrOwnerAlreadyStarted = errors.New("control session owner has already started")
	// ErrOwnerNotRunning 表示调用方在 Start 前调用 Enqueue 或 Wait。
	ErrOwnerNotRunning = errors.New("control session owner is not running")
	// ErrOwnerClosed 表示 Session 已开始关闭，不再接受新的发送消息。
	ErrOwnerClosed = errors.New("control session owner is closed")
	// ErrInboundQueueFull 表示业务方没有及时消费有界 Inbound 事件。
	ErrInboundQueueFull = errors.New("control session inbound queue is full")
	// ErrControlProtocol 表示对端消息或待发消息违反 Protocol v1。
	ErrControlProtocol = errors.New("control session protocol violation")
	// ErrControlRead 表示 Control 连接读取发生非协议类网络错误。
	ErrControlRead = errors.New("control session read failed")
	// ErrControlWrite 表示 Control Frame 未能在固定写超时内完整写出。
	ErrControlWrite = errors.New("control session write failed")
)

// Options 是一条已认证 Control Session 的固定内存与 IO 边界。
type Options struct {
	ProtocolVersion      uint32
	HighPriorityCapacity int
	NormalCapacity       int
	InboundCapacity      int
	WriteTimeout         time.Duration
	// MaxFrameBytes 由 Server 配置收紧；Agent 零值继续使用 Protocol v1 绝对上限。
	MaxFrameBytes uint64
}

// Inbound 是已经由 Owner 串行通过协议状态校验的入站消息。
// Envelope 的所有权在投递成功后转交消费者；Owner 不再读取或改写它。
type Inbound struct {
	Envelope  *protocolv1.ControlEnvelope
	Duplicate bool
}

type readEvent struct {
	envelope *protocolv1.ControlEnvelope
	err      error
}

// Owner 独占一条已认证 Control 连接及其非并发安全的协议状态。
//
// Start 固定创建一个 readLoop、一个 writeLoop 和一个中央 ownerLoop。只有 readLoop
// 调用 ReadControl，只有 writeLoop 调用 WriteControl，只有 ownerLoop 调用 Control
// 的 AcceptInbound、AcceptOutbound 与 Close。业务代码只能使用 Enqueue 和 Inbound，
// 永远不会取得 net.Conn，也不能绕过 Owner 直接修改协议状态。
type Owner struct {
	connection net.Conn
	control    *state.Control
	outbox     *Outbox
	options    Options

	inbound       chan Inbound
	wake          chan struct{}
	fatal         chan error
	readEvents    chan readEvent
	writeRequests chan *protocolv1.ControlEnvelope
	writeResults  chan error
	done          chan struct{}

	lifecycleMu sync.Mutex
	started     bool
	cancel      context.CancelFunc

	enqueueMu sync.Mutex
	accepting bool

	resultMu sync.Mutex
	result   error

	ioWait sync.WaitGroup
}

// NewOwner 构造尚未启动的 Control Session Owner。
// control 必须已经由 AUTH 成功提交到 ESTABLISHED；Owner 不负责认证或重连。
// Start 成功后才转移 connection 与 control 的独占所有权；调用方在 Done 关闭前
// 不得再直接读写连接或调用 Control 方法，否则会破坏 Single Reader/Writer/Owner 契约。
func NewOwner(connection net.Conn, control *state.Control, options Options) (*Owner, error) {
	if options.MaxFrameBytes == 0 {
		options.MaxFrameBytes = frame.MaxControlFrameSize
	}
	if connection == nil || control == nil || control.Phase() != state.ControlEstablished ||
		options.ProtocolVersion == 0 || options.HighPriorityCapacity <= 0 || options.NormalCapacity <= 0 ||
		options.InboundCapacity <= 0 || options.WriteTimeout <= 0 || options.MaxFrameBytes > frame.MaxControlFrameSize {
		return nil, ErrInvalidOwnerOptions
	}
	outbox, err := newOutbox(options.ProtocolVersion, options.HighPriorityCapacity, options.NormalCapacity, options.MaxFrameBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidOwnerOptions, err)
	}
	return &Owner{
		connection: connection,
		control:    control,
		outbox:     outbox,
		options:    options,
		inbound:    make(chan Inbound, options.InboundCapacity),
		wake:       make(chan struct{}, 1),
		fatal:      make(chan error, 1),
		readEvents: make(chan readEvent, 1),
		// 单槽只允许 ownerLoop 安排一个已通过状态校验的待写 Frame。
		writeRequests: make(chan *protocolv1.ControlEnvelope, 1),
		writeResults:  make(chan error, 1),
		done:          make(chan struct{}),
	}, nil
}

// Start 启动固定的三个内部循环并立即返回；同一个 Owner 只能启动一次。
func (owner *Owner) Start(parent context.Context) error {
	if parent == nil {
		return ErrInvalidOwnerOptions
	}
	owner.lifecycleMu.Lock()
	defer owner.lifecycleMu.Unlock()
	if owner.started {
		return ErrOwnerAlreadyStarted
	}
	ctx, cancel := context.WithCancel(parent)
	owner.started = true
	owner.cancel = cancel
	owner.enqueueMu.Lock()
	owner.accepting = true
	owner.enqueueMu.Unlock()

	owner.ioWait.Add(2)
	go owner.readLoop(ctx)
	go owner.writeLoop(ctx)
	go owner.ownerLoop(ctx)
	return nil
}

// Enqueue 深拷贝并立即尝试写入有界 Outbox，然后以非阻塞信号唤醒 ownerLoop。
// 返回 nil 只表示消息已进入当前 Session 的发送序列，不表示网络已经写出。
// 任一入队错误都会关闭 Session，防止调用方忽略错误后继续制造失序消息。
func (owner *Owner) Enqueue(envelope *protocolv1.ControlEnvelope) error {
	if !owner.startedState() {
		return ErrOwnerNotRunning
	}
	owner.enqueueMu.Lock()
	defer owner.enqueueMu.Unlock()
	if !owner.accepting {
		return ErrOwnerClosed
	}
	if err := owner.outbox.Enqueue(envelope); err != nil {
		owner.accepting = false
		owner.signalFatal(err)
		return err
	}
	select {
	case owner.wake <- struct{}{}:
	default:
		// wake 只表达“Outbox 非空”，已有待处理信号时无需累计。
	}
	return nil
}

// Inbound 返回只读、有界的入站事件流。
//
// ownerLoop 以非阻塞方式投递；消费者不得长时间停顿。容量耗尽会以
// ErrInboundQueueFull 关闭 Session，而不会让网络读循环或状态 Owner 被业务阻塞。
func (owner *Owner) Inbound() <-chan Inbound {
	return owner.inbound
}

// Done 在关闭顺序和全部内部 goroutine 退出后关闭。
func (owner *Owner) Done() <-chan struct{} {
	return owner.done
}

// Wait 等待全部内部 goroutine 退出并返回唯一终止原因。
// 在完整 Frame 边界收到普通 EOF 属于正常对端关闭，若本地关闭步骤无错误则返回 nil。
func (owner *Owner) Wait() error {
	if !owner.startedState() {
		return ErrOwnerNotRunning
	}
	<-owner.done
	owner.resultMu.Lock()
	defer owner.resultMu.Unlock()
	return owner.result
}

func (owner *Owner) ownerLoop(ctx context.Context) {
	var writeInFlight bool
	var terminal error

	for terminal == nil {
		// fatal 优先于继续调度已有消息，确保容量或调用错误尽快进入单一关闭路径。
		select {
		case terminal = <-owner.fatal:
			continue
		default:
		}

		if !writeInFlight {
			envelope, ok := owner.outbox.Dequeue()
			if ok {
				if _, err := owner.control.AcceptOutbound(envelope); err != nil {
					terminal = fmt.Errorf("%w: outbound: %w", ErrControlProtocol, err)
					continue
				}
				select {
				case owner.writeRequests <- envelope:
					writeInFlight = true
				case <-ctx.Done():
					terminal = ctx.Err()
					continue
				}
			}
		}

		select {
		case <-ctx.Done():
			terminal = ctx.Err()
		case err := <-owner.fatal:
			terminal = err
		case event := <-owner.readEvents:
			terminal = owner.acceptInbound(event)
		case err := <-owner.writeResults:
			if !writeInFlight {
				terminal = fmt.Errorf("%w: result without in-flight frame", ErrControlWrite)
				continue
			}
			if err != nil {
				terminal = fmt.Errorf("%w: %w", ErrControlWrite, err)
				continue
			}
			writeInFlight = false
		case <-owner.wake:
			// 下一轮从 Outbox 取出合并后的最终消息。
		}
	}

	owner.finish(terminal)
}

func (owner *Owner) acceptInbound(event readEvent) error {
	if event.err != nil {
		if errors.Is(event.err, io.EOF) {
			return io.EOF
		}
		if isFrameProtocolError(event.err) {
			return fmt.Errorf("%w: decode: %w", ErrControlProtocol, event.err)
		}
		return fmt.Errorf("%w: %w", ErrControlRead, event.err)
	}
	if err := validate.RejectUnknownFields(event.envelope); err != nil {
		return fmt.Errorf("%w: %w", ErrControlProtocol, err)
	}
	result, err := owner.control.AcceptInbound(event.envelope)
	if err != nil {
		return fmt.Errorf("%w: inbound: %w", ErrControlProtocol, err)
	}
	select {
	case owner.inbound <- Inbound{Envelope: event.envelope, Duplicate: result.Duplicate}:
		return nil
	default:
		return ErrInboundQueueFull
	}
}

func (owner *Owner) readLoop(ctx context.Context) {
	defer owner.ioWait.Done()
	for {
		envelope := &protocolv1.ControlEnvelope{}
		err := frame.ReadControlLimit(owner.connection, envelope, owner.options.MaxFrameBytes)
		event := readEvent{envelope: envelope, err: err}
		select {
		case owner.readEvents <- event:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func (owner *Owner) writeLoop(ctx context.Context) {
	defer owner.ioWait.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case envelope := <-owner.writeRequests:
			deadlineErr := owner.connection.SetWriteDeadline(time.Now().Add(owner.options.WriteTimeout))
			if deadlineErr != nil {
				owner.sendWriteResult(ctx, deadlineErr)
				return
			}
			err := frame.WriteControlLimit(owner.connection, envelope, owner.options.MaxFrameBytes)
			owner.sendWriteResult(ctx, err)
			if err != nil {
				return
			}
		}
	}
}

func (owner *Owner) sendWriteResult(ctx context.Context, err error) {
	select {
	case owner.writeResults <- err:
	case <-ctx.Done():
	}
}

func (owner *Owner) finish(cause error) {
	owner.enqueueMu.Lock()
	owner.accepting = false
	owner.enqueueMu.Unlock()

	// 单一关闭顺序：先广播取消，再用立即 Deadline 解除所有 IO，随后关闭连接，
	// 最后等待 readLoop/writeLoop。即使 Close 返回错误，也不能跳过等待。
	owner.cancel()
	deadlineErr := owner.connection.SetDeadline(time.Now())
	closeErr := owner.connection.Close()
	owner.control.Close()
	owner.ioWait.Wait()

	if errors.Is(cause, io.EOF) {
		cause = nil
	}
	owner.resultMu.Lock()
	owner.result = errors.Join(cause, wrapShutdownError("set deadline", deadlineErr), wrapShutdownError("close connection", closeErr))
	owner.resultMu.Unlock()
	close(owner.inbound)
	close(owner.done)
}

func (owner *Owner) signalFatal(err error) {
	select {
	case owner.fatal <- err:
	default:
	}
}

func (owner *Owner) startedState() bool {
	owner.lifecycleMu.Lock()
	defer owner.lifecycleMu.Unlock()
	return owner.started
}

func isFrameProtocolError(err error) bool {
	return errors.Is(err, frame.ErrInvalidLength) || errors.Is(err, frame.ErrTruncatedFrame) ||
		errors.Is(err, frame.ErrFrameTooLarge) || errors.Is(err, frame.ErrMalformedMessage) ||
		errors.Is(err, frame.ErrNilMessage)
}

func wrapShutdownError(operation string, err error) error {
	if err == nil || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return nil
	}
	return fmt.Errorf("control session shutdown %s: %w", operation, err)
}
