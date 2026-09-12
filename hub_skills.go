package thalovant

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// HubSkillWaitOptions controls optional polling after one accepted write.
// Zero durations use a 120-second timeout and two-second polling interval.
// Cancellation or polling failure never retries the accepted mutation.
type HubSkillWaitOptions struct {
	Wait         bool
	Timeout      time.Duration
	PollInterval time.Duration
}

func (o HubSkillWaitOptions) timings() (time.Duration, time.Duration, error) {
	if o.Timeout < 0 || o.PollInterval < 0 {
		return 0, 0, fmt.Errorf("%w: hub skill wait durations must not be negative", ErrAPI)
	}
	timeout, interval := o.Timeout, o.PollInterval
	if timeout == 0 {
		timeout = 120 * time.Second
	}
	if interval == 0 {
		interval = 2 * time.Second
	}
	return timeout, interval, nil
}
func hubSkillsPath(hubID string) string { return "/v1/hubs/" + url.PathEscape(hubID) + "/skills" }

// ListHubSkills reads the skills of the hub's shared runtime group.
func (c *ControlPlane) ListHubSkills(ctx context.Context, hubID string) (map[string]any, error) {
	return c.request(ctx, http.MethodGet, hubSkillsPath(hubID), nil, nil, true)
}

// ListHubSkillHistory reads newest-first events and operations. Limit is 1–200.
func (c *ControlPlane) ListHubSkillHistory(ctx context.Context, hubID string, limit int) (map[string]any, error) {
	if limit < 1 || limit > 200 {
		return nil, fmt.Errorf("%w: limit must be from 1 to 200", ErrAPI)
	}
	return c.request(ctx, http.MethodGet, fmt.Sprintf("%s/history?limit=%d", hubSkillsPath(hubID), limit), nil, nil, true)
}

// InstallHubSkill installs on the shared runtime; every hub using it is affected.
// Use version "latest" or an exact version. Requires hubs:write and a paid plan.
func (c *ControlPlane) InstallHubSkill(ctx context.Context, hubID, skill, version string, opts HubSkillWaitOptions) (map[string]any, error) {
	return c.changeHubSkill(ctx, http.MethodPost, hubSkillsPath(hubID), map[string]any{"skill": skill, "version": version}, opts)
}

// UpdateHubSkill moves a skill to an exact version or "latest" on the shared runtime.
func (c *ControlPlane) UpdateHubSkill(ctx context.Context, hubID, skill, version string, opts HubSkillWaitOptions) (map[string]any, error) {
	return c.changeHubSkill(ctx, http.MethodPatch, hubSkillsPath(hubID)+"/"+url.PathEscape(skill), map[string]any{"version": version}, opts)
}

// RemoveHubSkill removes the shared runtime attachment, affecting every served hub.
func (c *ControlPlane) RemoveHubSkill(ctx context.Context, hubID, skill string, opts HubSkillWaitOptions) (map[string]any, error) {
	return c.changeHubSkill(ctx, http.MethodDelete, hubSkillsPath(hubID)+"/"+url.PathEscape(skill), nil, opts)
}
func (c *ControlPlane) changeHubSkill(ctx context.Context, method, path string, body map[string]any, opts HubSkillWaitOptions) (map[string]any, error) {
	if _, _, err := opts.timings(); err != nil {
		return nil, err
	}
	accepted, err := c.request(ctx, method, path, body, nil, true)
	if err != nil {
		return nil, err
	}
	if !opts.Wait {
		return accepted, nil
	}
	return c.WaitForHubSkillOperation(ctx, accepted, opts)
}

// WaitForHubSkillOperation resumes an accepted write without repeating it.
// On failure the accepted response is also returned so its operation_id survives.
func (c *ControlPlane) WaitForHubSkillOperation(ctx context.Context, accepted map[string]any, opts HubSkillWaitOptions) (map[string]any, error) {
	timeout, interval, err := opts.timings()
	if err != nil {
		return accepted, err
	}
	id, _ := accepted["operation_id"].(string)
	if id == "" {
		return accepted, fmt.Errorf("%w: missing accepted operation_id", ErrAPI)
	}
	state, _ := accepted["state"].(string)
	converged := "installed"
	if state == "removing" || state == "removed" {
		converged = "removed"
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return accepted, fmt.Errorf("accepted operation %s: %w", id, err)
		}
		if !time.Now().Before(deadline) {
			return accepted, fmt.Errorf("%w: accepted operation %s", ErrTimeout, id)
		}
		operation, err := c.GetOperation(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return accepted, fmt.Errorf("accepted operation %s: %w", id, ctx.Err())
			}
			return accepted, fmt.Errorf("%w: could not read accepted operation %s; resume using its ID", ErrAPI, id)
		}
		switch operation.Status {
		case OperationReady:
			result := make(map[string]any, len(accepted)+1)
			for key, value := range accepted {
				result[key] = value
			}
			result["state"] = converged
			result["operation"] = operation
			return result, nil
		case OperationFailed, OperationTimedOut:
			return accepted, fmt.Errorf("%w: accepted operation %s ended with status %s; inspect GetOperation for details", ErrAPI, id, operation.Status)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return accepted, fmt.Errorf("%w: accepted operation %s", ErrTimeout, id)
		}
		delay := interval
		if remaining < delay {
			delay = remaining
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return accepted, fmt.Errorf("accepted operation %s: %w", id, ctx.Err())
		case <-timer.C:
		}
	}
}
