// Package session manages device registry and session lifecycle state.
// reconcile.go implements startup reconciliation per D-10/D-11: kills orphan
// app_process instances and removes stale reverse forward mappings left by
// previous gateway runs (e.g., after kill -9).
package session

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/pelni/adb-gateway/internal/adb"
)

// Reconciler performs startup reconciliation to clean up state left by
// previous gateway runs. After kill -9, orphan app_process instances and
// stale reverse forward mappings persist on devices. The reconciler
// enumerates all devices, kills orphans, and removes stale forwards
// using marker-based identification (D-10) so it never touches non-gateway
// processes or forwards (safe for coexisting ADB tools).
type Reconciler struct {
	hostSvc   *adb.HostServices
	adbClient *adb.Client
}

// NewReconciler creates a Reconciler with the given ADB dependencies.
func NewReconciler(hostSvc *adb.HostServices, adbClient *adb.Client) *Reconciler {
	return &Reconciler{
		hostSvc:   hostSvc,
		adbClient: adbClient,
	}
}

// Reconcile performs the startup reconciliation sequence per D-11:
//  1. Enumerate devices via hostSvc.ListDevices.
//  2. For each device: find and kill orphan app_process instances matching
//     gateway's jar CLASSPATH (scrcpy-server-gateway.jar) per D-10.
//  3. For each device: list reverse forwards and remove gateway-owned ones
//     (matching localabstract:scrcpy_* pattern) per D-10.
//  4. Log all actions at INFO level with device serial per D-11.
//
// Reconciliation is best-effort: errors are logged but do not prevent the
// gateway from starting. A partially-failed reconciliation is better than
// refusing to start.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	slog.Info("starting reconciliation of stale gateway state")

	devices, err := r.hostSvc.ListDevices(ctx)
	if err != nil {
		return fmt.Errorf("reconcile: enumerate devices: %w", err)
	}

	var reconcileErrors []error

	for _, dev := range devices {
		// Skip offline/unauthorized devices; we can't run shell commands on them.
		if dev.State != "device" {
			slog.Info("skipping reconciliation for non-available device",
				"device", dev.Serial,
				"state", dev.State,
			)
			continue
		}

		// Step 2: Kill orphan app_process instances (D-10/D-11)
		if err := r.killOrphans(ctx, dev.Serial); err != nil {
			slog.Error("reconcile: failed to kill orphans",
				"device", dev.Serial,
				"error", err,
			)
			reconcileErrors = append(reconcileErrors, err)
		}

		// Step 3: Remove stale reverse forwards (D-10/D-11)
		if err := r.removeStaleForwards(ctx, dev.Serial); err != nil {
			slog.Error("reconcile: failed to remove stale forwards",
				"device", dev.Serial,
				"error", err,
			)
			reconcileErrors = append(reconcileErrors, err)
		}
	}

	if len(reconcileErrors) > 0 {
		return fmt.Errorf("reconcile: %d error(s) encountered", len(reconcileErrors))
	}

	slog.Info("reconciliation complete")
	return nil
}

// killOrphans finds and kills app_process instances matching the gateway's
// jar CLASSPATH (/data/local/tmp/scrcpy-server-gateway.jar) on the device.
// Per D-10: only gateway-owned processes are killed, safe for coexisting ADB tools.
func (r *Reconciler) killOrphans(ctx context.Context, serial string) error {
	// Use ps to find app_process instances matching gateway's jar.
	// The -A flag lists all processes; -o PID,ARGS gives us PID and full command line.
	output, err := r.hostSvc.RunShellCommand(ctx, serial,
		"ps -A -o PID,ARGS | grep scrcpy-server-gateway.jar")
	if err != nil {
		return fmt.Errorf("shell ps grep: %w", err)
	}

	if output == "" {
		slog.Info("reconcile: no orphan processes found", "device", serial)
		return nil
	}

	// Parse output lines to extract PIDs.
	// ps output format: "PID ARGS\n"
	pids := parseOrphanPIDs(output)
	if len(pids) == 0 {
		slog.Info("reconcile: no orphan PIDs parsed", "device", serial, "raw_output", output)
		return nil
	}

	// Kill each orphan process.
	for _, pid := range pids {
		slog.Info("reconcile: killing orphan process",
			"device", serial,
			"pid", pid,
		)
		_, err := r.hostSvc.RunShellCommand(ctx, serial, fmt.Sprintf("kill %d", pid))
		if err != nil {
			slog.Warn("reconcile: failed to kill orphan",
				"device", serial,
				"pid", pid,
				"error", err,
			)
			// Continue trying to kill other orphans.
		}
	}

	return nil
}

// removeStaleForwards lists reverse forwards on the device and removes those
// owned by the gateway. Per D-10: only forwards matching the
// localabstract:scrcpy_* pattern are removed, preserving forwards created
// by other ADB tools.
func (r *Reconciler) removeStaleForwards(ctx context.Context, serial string) error {
	entries, err := r.adbClient.ReverseListForward(ctx, serial)
	if err != nil {
		return fmt.Errorf("reverse list forward: %w", err)
	}

	removed := 0
	for _, entry := range entries {
		if isGatewayOwned(entry) {
			slog.Info("reconcile: removing stale reverse forward",
				"device", serial,
				"local", entry.Local,
				"remote", entry.Remote,
			)
			if err := r.adbClient.ReverseRemove(ctx, serial, entry.Local); err != nil {
				slog.Warn("reconcile: failed to remove reverse forward",
					"device", serial,
					"local", entry.Local,
					"error", err,
				)
			} else {
				removed++
			}
		}
	}

	slog.Info("reconcile: stale forwards removed",
		"device", serial,
		"removed", removed,
		"total", len(entries),
	)
	return nil
}

// isGatewayOwned returns true if the forward entry was created by this gateway,
// identified by the localabstract:scrcpy_* marker pattern (D-10).
// Only gateway-owned forwards are removed during reconciliation, preserving
// forwards created by other ADB tools (safe coexistence).
func isGatewayOwned(entry adb.ForwardEntry) bool {
	return strings.HasPrefix(entry.Local, "localabstract:scrcpy_")
}

// parseOrphanPIDs extracts process IDs from ps output lines.
// Input format: lines like "  1234 CLASSPATH=/data/local/tmp/scrcpy-server-gateway.jar app_process ..."
// grep output also includes the grep line itself, which we filter out.
func parseOrphanPIDs(output string) []int {
	var pids []int
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Skip the grep process itself (contains "grep" in the command line).
		if strings.Contains(line, "grep") {
			continue
		}
		// Parse PID from the first field.
		fields := strings.Fields(line)
		if len(fields) < 1 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}