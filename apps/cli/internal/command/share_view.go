package command

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/shareclient"
)

func writeShareSessionHuman(writer io.Writer, created *shareclient.ShareCreated, now time.Time, loggedIn bool) {
	style := newTermStyle(writer)
	fmt.Fprintln(writer)
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Share"), style.boldCyan(created.PublicURL))
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Expires"), formatDisplayTime(now, created.Share.ExpiresAt.Format(time.RFC3339)))
	fmt.Fprintf(writer, "  %s  %s %s\n", style.field("Status"), style.boldCyan(arrowMark(style)), style.bold("Waiting for request"))
	cap := created.Share.CapabilityRevision.Name
	if cap != "" {
		fmt.Fprintf(writer, "  %s  %s\n", style.field("Cap"), cap)
	}
	if created.Share.Recipe != nil {
		fmt.Fprintf(writer, "  %s  %s\n", style.field("Recipe"), revisionLabel(created.Share.Recipe.Name, created.Share.Recipe.Version))
	}
	if !loggedIn {
		fmt.Fprintf(writer, "  %s  %s\n", style.field("Tip"), style.dim("doppels login — see this Share in the console"))
	}
	fmt.Fprintln(writer)
	fmt.Fprintf(writer, "  %s\n", style.dim("Ctrl+C to stop"))
}

func writeSharedRequest(writer io.Writer, request execution.RequestRecord) {
	style := newTermStyle(writer)
	fmt.Fprintln(writer)
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Request"), style.bold("from "+request.RequestedBy.ID))
	names := make([]string, 0, len(request.Inputs))
	for name := range request.Inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		encoded, err := json.Marshal(request.Inputs[name])
		if err != nil {
			encoded = []byte("<invalid>")
		}
		fmt.Fprintf(writer, "  %s  %s = %s\n", style.field("Input"), style.cyan(name), string(encoded))
	}
	fmt.Fprintln(writer)
}

func writeShareUploadProgress(writer io.Writer, filename string, sizeBytes int64) {
	style := newTermStyle(writer)
	fmt.Fprintf(writer, "  %s  %s  %s\n", style.field("Upload"), style.value(filename), style.dim(formatByteSize(sizeBytes)))
}

func writeLockPinCard(writer io.Writer, issues []lockPinIssue, strict bool) {
	if len(issues) == 0 {
		return
	}
	style := newTermStyle(writer)
	fmt.Fprintln(writer)
	if strict {
		fmt.Fprintf(writer, "  %s  %s\n", style.boldRed("✗"), style.bold("PIN stale"))
	} else {
		fmt.Fprintf(writer, "  %s  %s\n", style.boldYellow("!"), style.bold("PIN stale"))
	}
	fmt.Fprintln(writer)
	for index, issue := range issues {
		if index > 0 {
			fmt.Fprintln(writer)
		}
		kind := "Cap"
		if issue.Kind == "Recipe" {
			kind = "Recipe"
		}
		fmt.Fprintf(writer, "  %s  %s@%s\n", style.field(kind), style.value(issue.Name), issue.Version)
		if rel := listenRelPath(issue.Root, issue.Path); rel != "" {
			fmt.Fprintf(writer, "  %s  %s\n", style.field("File"), style.fileLink(issue.Path, rel))
		}
		fmt.Fprintf(writer, "  %s  %s\n", style.field("Why"), "changed without a version bump")
	}
	fmt.Fprintln(writer)
	next := "bump metadata.version and re-apply"
	if strict {
		next += ", or omit --strict"
	}
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Next"), next)
	fmt.Fprintln(writer)
}

func writeHostNotReady(writer io.Writer, recipeName, relPath string, missing []string) {
	style := newTermStyle(writer)
	fmt.Fprintln(writer)
	fmt.Fprintf(writer, "  %s  %s\n", style.boldRed("✗"), style.bold("Recipe not ready"))
	fmt.Fprintln(writer)
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Recipe"), style.value(listenDisplayName(recipeName)))
	if relPath != "" {
		fmt.Fprintf(writer, "  %s  %s\n", style.field("File"), style.dim(relPath))
	}
	if len(missing) == 0 {
		fmt.Fprintln(writer)
		return
	}
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, "  "+style.bold("Requires"))
	for _, need := range missing {
		fmt.Fprintf(writer, "    %s  %s\n", style.boldRed("✗"), need)
	}
	fmt.Fprintln(writer)
}

func writeCatalogHostFailures(writer io.Writer, catalog *manifest.Catalog, host manifest.Host) bool {
	if catalog == nil || host == nil {
		return false
	}
	names := make([]string, 0, len(catalog.Recipes))
	for name := range catalog.Recipes {
		names = append(names, name)
	}
	sort.Strings(names)
	wrote := false
	for _, name := range names {
		for _, recipe := range catalog.Recipes[name] {
			missing := manifest.CheckRequires(recipe.Value, catalog.Root, host)
			if len(missing) == 0 {
				continue
			}
			writeHostNotReady(writer, recipe.Value.Metadata.Name, listenRelPath(catalog.Root, recipe.Source.Path), missing)
			wrote = true
		}
	}
	return wrote
}

// waitSpinner shows a live indicator until Stop is called.
type waitSpinner struct {
	writer io.Writer
	style  termStyle
	stop   chan struct{}
	done   chan struct{}
	mu     sync.Mutex
	active bool
}

func startShareWaitSpinner(writer io.Writer) *waitSpinner {
	return startWaitSpinner(writer, "waiting for request")
}

func startWaitSpinner(writer io.Writer, label string) *waitSpinner {
	style := newTermStyle(writer)
	s := &waitSpinner{writer: writer, style: style}
	if !style.enabled {
		return s
	}
	if label == "" {
		label = "waiting"
	}
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	s.active = true
	started := time.Now()
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		frame := 0
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				elapsed := formatDurationLive(time.Since(started))
				s.mu.Lock()
				mark := s.style.cyan(spinnerFrames[frame%len(spinnerFrames)])
				msg := fmt.Sprintf("  %s %s  %s", mark, label, s.style.dim("· "+elapsed))
				// Erase line (\x1b[2K) then rewrite — bare \r stacks frames in
				// some terminals (Cursor / multiplexed stderr) when ANSI length
				// or stream merging leaves leftovers.
				fmt.Fprintf(s.writer, "\r\x1b[2K%s", msg)
				s.mu.Unlock()
				frame++
			}
		}
	}()
	return s
}

func (s *waitSpinner) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	active := s.active
	stop := s.stop
	done := s.done
	s.active = false
	s.mu.Unlock()
	if !active || stop == nil {
		return
	}
	close(stop)
	<-done
	s.mu.Lock()
	fmt.Fprint(s.writer, "\r\x1b[2K")
	s.mu.Unlock()
}
