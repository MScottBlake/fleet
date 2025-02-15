package service

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/fleetdm/fleet/v4/server/contexts/ctxerr"
	"github.com/fleetdm/fleet/v4/server/fleet"
)

func (svc *Service) GetHost(ctx context.Context, id uint, opts fleet.HostDetailOptions) (*fleet.HostDetail, error) {
	// reuse GetHost, but include premium details
	opts.IncludeCVEScores = true
	opts.IncludePolicies = true
	return svc.Service.GetHost(ctx, id, opts)
}

func (svc *Service) HostByIdentifier(ctx context.Context, identifier string, opts fleet.HostDetailOptions) (*fleet.HostDetail, error) {
	// reuse HostByIdentifier, but include premium options
	opts.IncludeCVEScores = true
	opts.IncludePolicies = true
	return svc.Service.HostByIdentifier(ctx, identifier, opts)
}

func (svc *Service) RunHostScript(ctx context.Context, request *fleet.HostScriptRequestPayload, waitForResult time.Duration) (*fleet.HostScriptResult, error) {
	const (
		maxScriptRuneLen    = 10000
		maxPendingScriptAge = time.Minute // any script older than this is not considered pending anymore on that host
	)

	// must load the host (lite is enough, just for the team) to authorize
	// with the proper team id. We cannot first authorize if the user can list
	// hosts, because the user could have a write-only role (e.g. gitops).
	host, err := svc.ds.HostLite(ctx, request.HostID)
	if err != nil {
		// if error is because the host does not exist, check first if the user
		// had access to run a script (to prevent leaking valid host ids).
		if fleet.IsNotFound(err) {
			if err := svc.authz.Authorize(ctx, &fleet.HostScriptResult{}, fleet.ActionWrite); err != nil {
				return nil, err
			}
		}
		svc.authz.SkipAuthorization(ctx)
		return nil, ctxerr.Wrap(ctx, err, "get host lite")
	}
	if err := svc.authz.Authorize(ctx, &fleet.HostScriptResult{TeamID: host.TeamID}, fleet.ActionWrite); err != nil {
		return nil, err
	}

	if request.ScriptContents == "" {
		return nil, fleet.NewInvalidArgumentError("script_contents", "a script to execute is required")
	}
	// look for the script length in bytes first, as rune counting a huge string
	// can be expensive.
	if len(request.ScriptContents) > utf8.UTFMax*maxScriptRuneLen {
		return nil, fleet.NewInvalidArgumentError("script_contents", fmt.Sprintf("script is too long, must be at most %d characters", maxScriptRuneLen))
	}
	// now that we know that the script is at most 4*maxScriptRuneLen bytes long,
	// we can safely count the runes for a precise check.
	if utf8.RuneCountInString(request.ScriptContents) > maxScriptRuneLen {
		return nil, fleet.NewInvalidArgumentError("script_contents", fmt.Sprintf("script is too long, must be at most %d characters", maxScriptRuneLen))
	}

	// TODO(mna): any other validation we want to apply to the script? What is the "must be bash/powershell" check?

	pending, err := svc.ds.ListPendingHostScriptExecutions(ctx, request.HostID, maxPendingScriptAge)
	if err != nil {
		return nil, ctxerr.Wrap(ctx, err, "list host pending script executions")
	}
	if len(pending) > 0 {
		// TODO(mna): there are a number of issues with that validation: it only
		// really says that there was a script execution _request_ that was made < 1m
		// ago, and that blocks executing any more scripts on that host, but the
		// host may not even have received the previous script for execution yet,
		// so if we accept more scripts after 1m, we may end up having multiple
		// scripts to execute on the host at the same time (or more likely in
		// sequence, but still). This may be good enough for now, I think the whole
		// idea of locking if a script is pending is meant to be temporary anyway.
		return nil, fleet.NewInvalidArgumentError("script_contents", "a script is currently executing on the host")
	}

	// create the script execution request
	script, err := svc.ds.NewHostScriptExecutionRequest(ctx, request)
	if err != nil {
		return nil, ctxerr.Wrap(ctx, err, "create script execution request")
	}
	// TODO(mna): figure out how to send this to the host, either something to do
	// here or via the DB checking if there are pending scripts for the host when
	// sending queries or notifications.
	if waitForResult <= 0 {
		// async execution, return
		return script, nil
	}

	ctx, cancel := context.WithTimeout(ctx, waitForResult)
	defer cancel()

	// if waiting for a result times out, we still want to return the script's
	// execution request information along with the error, so that the caller can
	// use the execution id for later checks.
	timeoutResult := script
	checkInterval := time.Second
	after := time.NewTimer(checkInterval)
	for {
		select {
		case <-ctx.Done():
			return timeoutResult, ctx.Err()
		case <-after.C:
			result, err := svc.ds.GetHostScriptExecutionResult(ctx, script.ExecutionID)
			if err != nil {
				// is that due to the context being canceled during the DB access?
				if ctxErr := ctx.Err(); ctxErr != nil {
					return timeoutResult, ctxErr
				}
				return nil, ctxerr.Wrap(ctx, err, "get script execution result")
			}
			if result.ExitCode.Valid {
				// a result was received from the host, return
				return result, nil
			}

			// at a second to every attempt, until it reaches 5s (then check every 5s)
			if checkInterval < 5*time.Second {
				checkInterval += time.Second
			}
			after.Reset(checkInterval)
		}
	}
}
