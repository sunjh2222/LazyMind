package systemdeps

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"time"

	appLog "lazymind/core/log"
)

// Download addresses and revisions come from the installed app's bound
// catalog. The durable record contains only which optional components to keep.
type selectedComponentUpdate struct {
	ReleaseID       string            `json:"releaseId"`
	CatalogSHA256   string            `json:"componentsSHA256"`
	TransactionID   string            `json:"transactionId"`
	Wanted          []string          `json:"wanted"`
	SourceRevisions map[string]string `json:"sourceRevisions,omitempty"`
	Verified        map[string]string `json:"verifiedComponents,omitempty"`
}

func (want selectedComponentUpdate) sameRelease(other selectedComponentUpdate) bool {
	return want.ReleaseID == other.ReleaseID && want.CatalogSHA256 == other.CatalogSHA256 &&
		want.TransactionID == other.TransactionID
}

func (want selectedComponentUpdate) save(root string) error {
	raw, err := json.Marshal(want)
	if err != nil {
		return err
	}
	return componentAtomicWrite(selectionFile(root), raw)
}

func (env *PythonComponentMaintenanceContext) VerifyInstalledRelease(ctx context.Context) error {
	helperName := "lazymind-update-helper"
	if runtime.GOOS == "windows" {
		helperName += ".exe"
	}
	// Allow the helper's bounded 60-second signature verification to finish.
	bounded, cancel := context.WithTimeout(ctx, 75*time.Second)
	defer cancel()
	output, err := exec.CommandContext(bounded, filepath.Join(env.ResourcesRoot, "bin", helperName), "verify-identity").Output()
	var identity struct {
		Status string
		Config struct{ ReleaseID string }
	}
	if err != nil || len(output) > 1024*1024 || json.Unmarshal(output, &identity) != nil ||
		identity.Status != "enabled" || identity.Config.ReleaseID == "" {
		return errors.New("installed component runtime identity unavailable")
	}
	env.ReleaseID = identity.Config.ReleaseID
	return nil
}

func selectionFile(runtimeRoot string) string {
	return filepath.Join(runtimeRoot, "deps", "python-components", "selected-update.json")
}

func deferSelectedComponents(env PythonComponentMaintenanceContext, record componentMaintenanceRecord) error {
	invalid := errors.New("component_state_unknown")
	if record.RepairTarget != nil && !pythonComponentDigest.MatchString(record.RepairTarget.AcceptanceDigest) {
		return invalid
	}
	catalog, err := readMaintenanceCatalog(record)
	if err != nil {
		return err
	}
	installed, err := componentFileBytes(filepath.Join(env.ResourcesRoot, "config", "python-components.json"), 1024*1024)
	if err != nil {
		return invalid
	}
	if _, err := parseBoundComponentCatalog(installed, record.ComponentsSHA256); err != nil {
		return err
	}
	updates := record.ComponentUpdates
	if record.SourceComponent != nil {
		if updates != nil || record.TargetComponent == nil {
			return invalid
		}
		updates = map[string]componentMaintenanceUpdate{"rag": {Source: *record.SourceComponent, Target: *record.TargetComponent}}
	}
	want := selectedComponentUpdate{ReleaseID: env.ReleaseID, CatalogSHA256: record.ComponentsSHA256, TransactionID: record.TransactionID, Wanted: []string{}}
	wanted := record.WantedComponents
	if wanted == nil {
		wanted = slices.Sorted(maps.Keys(updates))
	}
	if len(wanted) != len(updates) {
		return invalid
	}
	for i, id := range wanted {
		update, ok := updates[id]
		if !ok || pythonComponentRuleFor(id) == nil || slices.Contains(wanted[:i], id) || (update.Source.ID != id && (record.WantedComponents == nil || update.Source.ID != "")) {
			return invalid
		}
		target, exists := catalog.Components[id]
		expected := update.Target
		if repair := record.RepairTarget; repair != nil {
			expected = repair.TargetComponents[id]
			if id == "rag" && repair.TargetComponent != nil {
				expected = *repair.TargetComponent
			}
		}
		if exists != (expected.ID != "") || (exists && !reflect.DeepEqual(target, expected)) {
			return invalid
		}
		want.Wanted = append(want.Wanted, id)
		if update.Source.Revision != "" {
			if want.SourceRevisions == nil {
				want.SourceRevisions = map[string]string{}
			}
			want.SourceRevisions[id] = update.Source.Revision
		}
	}
	if slices.Contains(want.Wanted, "rag") != record.SourceHadRAG {
		return invalid
	}
	slices.Sort(want.Wanted)
	if previous, _, err := env.selectedCatalog(); err == nil && previous.sameRelease(want) && slices.Equal(previous.Wanted, want.Wanted) {
		return nil
	}
	if err := want.save(env.RuntimeRoot); err != nil {
		return invalid
	}
	return nil
}

func readSelectedComponentUpdate(root string) (selectedComponentUpdate, error) {
	var want selectedComponentUpdate
	raw, err := componentFileBytes(selectionFile(root), 4096)
	if err != nil {
		return want, err
	}
	if json.Unmarshal(raw, &want) != nil || want.ReleaseID == "" || len(want.TransactionID) != 32 {
		return want, errors.New("component_state_unknown")
	}
	if _, err := hex.DecodeString(want.TransactionID); err != nil {
		return want, err
	}
	for i, id := range want.Wanted {
		if pythonComponentRuleFor(id) == nil || slices.Contains(want.Wanted[:i], id) {
			return want, errors.New("component_state_unknown")
		}
	}
	return want, nil
}

func installedBoundCatalog(resources, digest string) (maintenanceCatalog, error) {
	raw, err := componentFileBytes(filepath.Join(resources, "config", "python-components.json"), 1024*1024)
	if err != nil {
		return maintenanceCatalog{}, err
	}
	return parseBoundComponentCatalog(raw, digest)
}

func (env *PythonComponentMaintenanceContext) selectedCatalog() (selectedComponentUpdate, maintenanceCatalog, error) {
	want, err := readSelectedComponentUpdate(env.RuntimeRoot)
	if err != nil {
		return want, maintenanceCatalog{}, err
	}
	if want.ReleaseID != env.ReleaseID {
		return want, maintenanceCatalog{}, errors.New("update_transaction_conflict")
	}
	catalog, err := installedBoundCatalog(env.ResourcesRoot, want.CatalogSHA256)
	return want, catalog, err
}

// A later explicit installation on the packaged tag supersedes the pending
// choice. Only reuse an existing, locally validated overlay in that case.
func (want selectedComponentUpdate) target(root string, pin PythonComponent) PythonComponent {
	selected := installedPythonDescriptor(root, pin)
	if selected.Revision != pin.Revision && (want.Verified[pin.ID] != "" || selected.Revision != want.SourceRevisions[pin.ID]) {
		return selected
	}
	return pin
}

func (want selectedComponentUpdate) ready(root string, pin, target PythonComponent) bool {
	// An old receipt records a past verification. Without content evidence
	// the worker must still probe the local overlay on this startup.
	return want.Verified[target.ID] == target.Revision && exactComponentSelection(root, target) &&
		(target.Revision != pin.Revision || selectedComponentContentMatches(pythonComponentDir(root, target), target))
}

func reusablePinnedComponent(root string, target PythonComponent) bool {
	// An old fixed-URL installation can have a ready payload without a
	// catalog receipt. A missing marker permits a probe of that layout.
	return exactComponentSelection(root, target) &&
		(selectedComponentContentMatches(pythonComponentDir(root, target), target) || selectedComponentContentMissing(root, target))
}

// This read-only preflight avoids a full app signature check on every boot of
// a desktop that has already verified all installed optional components.
func HasPendingSelectedComponentWork(root, resources string) (bool, error) {
	if _, err := os.Lstat(selectionFile(root)); os.IsNotExist(err) {
		return false, nil
	}
	want, err := readSelectedComponentUpdate(root)
	if err != nil {
		return false, err
	}
	catalog, err := installedBoundCatalog(resources, want.CatalogSHA256)
	if err != nil {
		return false, err
	}
	for _, id := range want.Wanted {
		if pin, ok := catalog.Components[id]; ok {
			target := want.target(root, pin)
			if !want.ready(root, pin, target) {
				return true, nil
			}
		}
	}
	return false, nil
}

// Run once per selected component so an offline mirror for one component
// cannot delay another. A subsequent Core process reopens the same record.
func (env *PythonComponentMaintenanceContext) ReconcileSelectedPythonComponents(ctx context.Context, initialRetry time.Duration) error {
	want, catalog, err := env.selectedCatalog()
	if err != nil {
		return err
	}
	var workers sync.WaitGroup
	for _, id := range want.Wanted {
		entry, available := catalog.Components[id]
		if !available {
			continue
		}
		workers.Add(1)
		go func(target PythonComponent) {
			defer workers.Done()
			delay := initialRetry
			if delay <= 0 {
				delay = time.Second
			}
			for ctx.Err() == nil {
				current, _, err := env.selectedCatalog()
				if err != nil || !current.sameRelease(want) || !slices.Contains(current.Wanted, target.ID) {
					return
				}
				chosen := current.target(env.RuntimeRoot, target)
				if current.ready(env.RuntimeRoot, target, chosen) {
					return
				}
				err = env.reconcileComponent(ctx, current, catalog.RuntimeBaseFingerprint, target, chosen)
				if err == nil || ctx.Err() != nil {
					return
				}
				appLog.Logger.Warn().Str("component", target.ID).Dur("retry", delay).Err(err).Msg("component.update.retry")
				select {
				case <-ctx.Done():
					return
				case <-time.After(delay):
				}
				delay = min(5*time.Minute, 2*delay)
			}
		}(entry)
	}
	workers.Wait()
	return ctx.Err()
}

func (env *PythonComponentMaintenanceContext) reconcileComponent(ctx context.Context, want selectedComponentUpdate, fingerprint string, pin, target PythonComponent) error {
	active := filepath.Join(env.UpdaterRoot, "native-v2", "active.json")
	if _, err := os.Lstat(active); !os.IsNotExist(err) {
		return errors.New("update_transaction_busy")
	}
	root, err := os.OpenRoot(env.RuntimeRoot)
	if err != nil {
		return err
	}
	defer root.Close()
	// Per-component locking lets a new worker reclaim a previous process's
	// partial transfer, without blocking another component or an app update.
	id := target.ID
	parent := filepath.Join("deps", "python-components", ".selected-stages", id)
	if err := root.MkdirAll(parent, 0700); err != nil {
		return err
	}
	for dir := parent; dir != "."; dir = filepath.Dir(dir) {
		fi, err := root.Lstat(dir)
		if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return errors.New("component_path_invalid")
		}
	}
	lockPath := filepath.Join(parent, "writer.lock")
	lock, err := root.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	locked, err := tryComponentLock(lock)
	if err != nil || !locked {
		lock.Close()
		return errors.New("update_transaction_busy")
	}
	defer releaseComponentLock(lock)
	work := filepath.Join(parent, "work")
	if err := root.RemoveAll(work); err != nil {
		return err
	}
	defer root.RemoveAll(work)
	updates := map[string]componentMaintenanceUpdate{id: {Target: target}}
	pinned := target.Revision == pin.Revision
	final := pythonComponentDir(env.RuntimeRoot, target)
	probe := func(path string) error {
		probes := map[string]componentProbeInput{id: {Path: filepath.Join(path, "site-packages"), Descriptor: target}}
		return verifyTargetComponents(ctx, probes, fingerprint, *env)
	}
	reuse := !pinned || reusablePinnedComponent(env.RuntimeRoot, target)
	if reuse {
		if !pinned && !exactComponentSelection(env.RuntimeRoot, target) {
			return errors.New("component_selection_changed")
		}
		if err := probe(final); err != nil {
			if !pinned || ctx.Err() != nil {
				return err
			}
			reuse = false
		}
	}
	var stage componentMaintenanceStageItem
	path := final
	if reuse {
		rel, err := filepath.Rel(env.RuntimeRoot, final)
		if err != nil {
			return err
		}
		stage = componentMaintenanceStageItem{StagePath: rel, Revision: target.Revision, SHA256: target.SHA256}
	} else {
		stage, err = downloadTargetComponentIn(ctx, *env, root, target, work)
		if err != nil {
			return err
		}
		path = filepath.Join(env.RuntimeRoot, stage.StagePath)
		if err := writeSelectedComponentContent(path, target); err != nil {
			return err
		}
		if err := probe(path); err != nil {
			return err
		}
	}
	state, err := componentMaintenanceStateLock(ctx, env.UpdaterRoot)
	if err != nil {
		return err
	}
	defer releaseComponentLock(state)
	writer, err := acquireComponentWriter(env.RuntimeRoot)
	if err != nil {
		return err
	}
	defer releaseComponentLock(writer)
	if _, err := os.Lstat(active); !os.IsNotExist(err) {
		return errors.New("update_transaction_busy")
	}
	current, _, err := env.selectedCatalog()
	if err != nil || !current.sameRelease(want) || !slices.Contains(current.Wanted, target.ID) || !reflect.DeepEqual(current.target(env.RuntimeRoot, pin), target) {
		return errors.New("update_transaction_conflict")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	stages := map[string]componentMaintenanceStageItem{id: stage}
	switch {
	case pinned && reuse:
		if !reusablePinnedComponent(env.RuntimeRoot, target) {
			return errors.New("component_selection_changed")
		}
		if current.Verified[id] == target.Revision {
			return nil
		}
	case pinned:
		if !selectedComponentContentMatches(path, target) {
			return errors.New("component_file_invalid")
		}
		if !exactComponentSelection(env.RuntimeRoot, target) || !selectedComponentContentMatches(final, target) {
			if err := replaceSelectedComponent(env.RuntimeRoot, root, parent, target, stages, updates); err != nil {
				return err
			}
		}
	default:
		if err := activateTargetComponents(env.RuntimeRoot, root, stages, updates, []string{id}); err != nil {
			return err
		}
	}
	if current.Verified == nil {
		current.Verified = map[string]string{}
	}
	current.Verified[id] = target.Revision
	return current.save(env.RuntimeRoot)
}

// Keep the previous directory intact until the fixed ZIP passes verification.
// This scoped replacement runs under the state and component writer locks.
func replaceSelectedComponent(runtimeRoot string, root *os.Root, parent string, target PythonComponent, stages map[string]componentMaintenanceStageItem, updates map[string]componentMaintenanceUpdate) error {
	final := filepath.Join("deps", "python-components", target.ID, target.Revision)
	for dir := final; dir != "."; dir = filepath.Dir(dir) {
		info, err := root.Lstat(dir)
		if !os.IsNotExist(err) && (err != nil || info.Mode().Type() != os.ModeDir) {
			return errors.New("component_path_invalid")
		}
	}
	var backup string
	if _, err := root.Lstat(final); err == nil {
		previous, err := os.MkdirTemp(filepath.Join(runtimeRoot, parent), "previous-")
		if err != nil {
			return err
		}
		backup = filepath.Join(parent, filepath.Base(previous))
		if err := root.Rename(final, filepath.Join(backup, "payload")); err != nil {
			_ = root.Remove(backup)
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	err := activateTargetComponents(runtimeRoot, root, stages, updates, []string{target.ID})
	if backup == "" {
		return err
	}
	if err != nil {
		if rollback := root.RemoveAll(final); rollback != nil {
			return errors.Join(err, rollback)
		}
		if rollback := root.Rename(filepath.Join(backup, "payload"), final); rollback != nil {
			return errors.Join(err, rollback)
		}
	}
	_ = root.RemoveAll(backup)
	return err
}

// The explicit install path already owns the component writer lock. A later
// user choice belongs to the same B selection only after B verifies it.
func installRequestedPythonComponent(ctx context.Context, env PythonComponentMaintenanceContext, entry PythonComponent) error {
	file := selectionFile(env.RuntimeRoot)
	if _, err := os.Lstat(file); os.IsNotExist(err) {
		return installPythonComponent(ctx, env.RuntimeRoot, entry, entry.URL)
	} else if err != nil {
		return err
	}
	if err := env.VerifyInstalledRelease(ctx); err != nil {
		return err
	}
	want, catalog, err := env.selectedCatalog()
	if err != nil {
		return err
	}
	pin, ok := catalog.Components[entry.ID]
	if !ok {
		return errors.New("component_state_unknown")
	}
	if err := installPythonComponent(ctx, env.RuntimeRoot, entry, entry.URL); err != nil {
		return err
	}
	target := installedPythonDescriptor(env.RuntimeRoot, pin)
	if !pythonComponentInstalled(env.RuntimeRoot, target) {
		return errors.New("component_state_unknown")
	}
	if want.Verified[target.ID] == target.Revision && slices.Contains(want.Wanted, target.ID) {
		return nil
	}
	probe := componentProbeInput{Path: filepath.Join(pythonComponentDir(env.RuntimeRoot, target), "site-packages"), Descriptor: target}
	if err := verifyTargetComponents(ctx, map[string]componentProbeInput{target.ID: probe}, catalog.RuntimeBaseFingerprint, env); err != nil {
		return err
	}
	if !slices.Contains(want.Wanted, target.ID) {
		want.Wanted = append(want.Wanted, target.ID)
		slices.Sort(want.Wanted)
	}
	if want.Verified == nil {
		want.Verified = map[string]string{}
	}
	want.Verified[target.ID] = target.Revision
	return want.save(env.RuntimeRoot)
}
