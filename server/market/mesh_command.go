// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package market

import (
	"context"
	"fmt"
	"time"

	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/server/comms"
	"decred.org/dcrdex/server/mesh"
)

const (
	commandKindLimit           = "limit"
	commandKindMarket          = "market"
	commandKindCancel          = "cancel"
	commandKindScheduleSuspend = "schedule_suspend"
	commandKindScheduleResume  = "schedule_resume"

	lifecycleCommandTimeout = 2 * time.Minute
)

// Commands returns the mesh command handlers. Client routes submit them
// with ExecuteCommand; they do not write the DB themselves.
func (r *OrderRouter) Commands() map[string]mesh.CommandExecutor {
	return map[string]mesh.CommandExecutor{
		commandKindLimit:  r.executeLimit,
		commandKindMarket: r.executeMarket,
		commandKindCancel: r.executeCancel,
	}
}

type scheduleSuspendRequest struct {
	Market      string `json:"market"`
	TimeMs      int64  `json:"timeMs,omitempty"`
	PersistBook *bool  `json:"persistBook,omitempty"`
}

type scheduleSuspendResult struct {
	EpochIdx int64 `json:"epochIdx"`
	EndMs    int64 `json:"endMs"`
}

type scheduleResumeRequest struct {
	Market string `json:"market"`
	TimeMs int64  `json:"timeMs,omitempty"`
}

type scheduleResumeResult struct {
	EpochIdx int64 `json:"epochIdx"`
	StartMs  int64 `json:"startMs"`
}

type commandRunner interface {
	ExecuteCommand(context.Context, mesh.CommandRequest) *msgjson.Error
}

// LifecycleCommands returns the mesh command handlers for admin suspend and
// resume. The master runs them; a slave forwards.
func LifecycleCommands(markets map[string]*Market) map[string]mesh.CommandExecutor {
	return map[string]mesh.CommandExecutor{
		commandKindScheduleSuspend: func(cmdCtx *mesh.CommandContext) *msgjson.Error {
			return executeScheduleSuspend(cmdCtx, markets)
		},
		commandKindScheduleResume: func(cmdCtx *mesh.CommandContext) *msgjson.Error {
			return executeScheduleResume(cmdCtx, markets)
		},
	}
}

func executeScheduleSuspend(cmdCtx *mesh.CommandContext, markets map[string]*Market) *msgjson.Error {
	var req scheduleSuspendRequest
	if err := cmdCtx.Request.Msg.Unmarshal(&req); err != nil {
		return msgjson.NewError(msgjson.RPCParseError, "error parsing schedule_suspend request")
	}
	mkt := markets[req.Market]
	if mkt == nil {
		return msgjson.NewError(msgjson.UnknownMarketError, "unknown market %s", req.Market)
	}
	persistBook := true
	if req.PersistBook != nil {
		persistBook = *req.PersistBook
	}
	event, susp, err := mkt.ScheduleSuspendEvent(unixMilliOrZero(req.TimeMs), persistBook)
	if err != nil {
		return msgjson.NewError(msgjson.RPCInternalError, "failed to schedule suspend: %v", err)
	}
	if err = cmdCtx.Completion.Emit(cmdCtx.Context, event, func() any {
		return &scheduleSuspendResult{EpochIdx: susp.Idx, EndMs: susp.End.UnixMilli()}
	}); err != nil {
		mesh.LogApplyFailure(log, err, "Failed to apply schedule_suspend for market %s: %v", req.Market, err)
		return mesh.ClientError(err, msgjson.RPCInternalError, "failed to apply schedule_suspend: %v", err)
	}
	return nil
}

func executeScheduleResume(cmdCtx *mesh.CommandContext, markets map[string]*Market) *msgjson.Error {
	var req scheduleResumeRequest
	if err := cmdCtx.Request.Msg.Unmarshal(&req); err != nil {
		return msgjson.NewError(msgjson.RPCParseError, "error parsing schedule_resume request")
	}
	mkt := markets[req.Market]
	if mkt == nil {
		return msgjson.NewError(msgjson.UnknownMarketError, "unknown market %s", req.Market)
	}
	event, startEpoch, startTime, err := mkt.ScheduleResumeEvent(unixMilliOrZero(req.TimeMs))
	if err != nil {
		return msgjson.NewError(msgjson.RPCInternalError, "failed to schedule resume: %v", err)
	}
	if err = cmdCtx.Completion.Emit(cmdCtx.Context, event, func() any {
		return &scheduleResumeResult{EpochIdx: startEpoch, StartMs: startTime.UnixMilli()}
	}); err != nil {
		mesh.LogApplyFailure(log, err, "Failed to apply schedule_resume for market %s: %v", req.Market, err)
		return mesh.ClientError(err, msgjson.RPCInternalError, "failed to apply schedule_resume: %v", err)
	}
	return nil
}

func unixMilliOrZero(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// ExecuteScheduleSuspend runs schedule_suspend on the master (forwarded from
// a slave). persistBook is always sent; HTTP already defaults omitted persist
// to true.
func ExecuteScheduleSuspend(ctx context.Context, run commandRunner, market string, asSoonAs time.Time, persistBook bool) (*SuspendEpoch, error) {
	req := &scheduleSuspendRequest{
		Market:      market,
		PersistBook: &persistBook,
	}
	if !asSoonAs.IsZero() {
		req.TimeMs = asSoonAs.UnixMilli()
	}
	var result scheduleSuspendResult
	if err := executeLifecycleCommand(ctx, run, commandKindScheduleSuspend, req, &result); err != nil {
		return nil, err
	}
	if result.EpochIdx == 0 {
		return nil, fmt.Errorf("schedule_suspend returned zero epoch")
	}
	return &SuspendEpoch{Idx: result.EpochIdx, End: time.UnixMilli(result.EndMs)}, nil
}

// ExecuteScheduleResume runs schedule_resume on the master (forwarded from
// a slave).
func ExecuteScheduleResume(ctx context.Context, run commandRunner, market string, asSoonAs time.Time) (startEpoch int64, startTime time.Time, err error) {
	req := &scheduleResumeRequest{Market: market}
	if !asSoonAs.IsZero() {
		req.TimeMs = asSoonAs.UnixMilli()
	}
	var result scheduleResumeResult
	if err := executeLifecycleCommand(ctx, run, commandKindScheduleResume, req, &result); err != nil {
		return 0, time.Time{}, err
	}
	if result.EpochIdx == 0 {
		return 0, time.Time{}, fmt.Errorf("schedule_resume returned zero epoch")
	}
	return result.EpochIdx, time.UnixMilli(result.StartMs), nil
}

func executeLifecycleCommand(ctx context.Context, run commandRunner, kind string, payload, result any) error {
	if run == nil {
		return fmt.Errorf("mesh service is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cmdCtx, cancel := context.WithTimeout(ctx, lifecycleCommandTimeout)
	defer cancel()

	msg, err := msgjson.NewRequest(comms.NextID(), kind, payload)
	if err != nil {
		return err
	}

	responses := make(chan *msgjson.Message, 1)
	execErrs := make(chan *msgjson.Error, 1)
	// ExecuteCommand can return before Respond (slave forward), so wait on both.
	go func() {
		execErrs <- run.ExecuteCommand(cmdCtx, mesh.CommandRequest{
			Kind: kind,
			Msg:  msg,
			Respond: func(resp *msgjson.Message) error {
				select {
				case responses <- resp:
				default:
				}
				return nil
			},
		})
	}()

	resultFromResponse := func(resp *msgjson.Message) error {
		if resp == nil {
			return fmt.Errorf("nil %s response", kind)
		}
		return resp.UnmarshalResult(result)
	}

	select {
	case rpcErr := <-execErrs:
		if rpcErr != nil {
			return rpcErr
		}
	case resp := <-responses:
		return resultFromResponse(resp)
	case <-cmdCtx.Done():
		return cmdCtx.Err()
	}

	// ExecuteCommand returned nil. The result may already be buffered (master)
	// or still in flight (slave).
	select {
	case resp := <-responses:
		return resultFromResponse(resp)
	case <-cmdCtx.Done():
		select {
		case resp := <-responses:
			return resultFromResponse(resp)
		default:
			return cmdCtx.Err()
		}
	}
}
