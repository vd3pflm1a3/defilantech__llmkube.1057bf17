/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agent

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// lineSuffixPattern matches the trailing line citation on a file reference
// (`main.go:135`, `main.go:49-63`). path.Ext reads such a token's extension
// as ".go:135", so the suffix has to come off before the extension lookup
// or every line-cited reference is invisible to the extractor.
var lineSuffixPattern = regexp.MustCompile(`:\d+(?:-\d+)?$`)

// pathRefExtensions are the file extensions a token must carry to count
// as a concrete path reference in an issue body. The set is deliberately
// small: under-extracting is safe (no refs means the scope check stays
// observe-only) while over-extracting risks false drift flags, so
// identifiers, commands, and API groups must not slip through.
//
// This is the language-agnostic base: docs, config, and Go's own source
// extension (LLMKube's home turf). A repo's PRIMARY source language is
// declared per-task via GateProfile.SourceExtensions and unioned in at
// extraction time (see extractIssuePathRefs), so a Godot repo's `.gd`
// files or a Rust repo's `.rs` files are recognized as issue refs
// exactly as the diff-side hasSourceFile guard already recognizes them.
// Without that union the extractor was blind to every non-Go language,
// so the scope-overlap vouch (#744) never fired for the polyglot fleet.
var pathRefExtensions = map[string]bool{
	"go": true, "md": true, "yaml": true, "yml": true, "sh": true,
	"json": true, "mod": true, "sum": true, "tmpl": true, "proto": true,
	"toml": true, "mk": true, "txt": true, "hcl": true,
}

// bareSourceFilenames are build files an issue cites by name because they
// carry no extension, so path.Ext yields "" and the extension lookup can
// never admit them. Kept to unambiguous, conventionally-capitalized build
// entrypoints: a token like "Makefile" in an issue body is a file, not
// prose, whereas a general "capitalized word" rule would admit sentences.
var bareSourceFilenames = map[string]bool{
	"Dockerfile": true, "Makefile": true, "Justfile": true,
	"Containerfile": true, "Earthfile": true,
}

// extensionSet normalizes a GateProfile SourceExtensions list (".gd",
// ".TS") into the dotless-lowercase keys isPathRef compares against.
// Returns nil for an empty list so callers can cheaply skip the union.
func extensionSet(exts []string) map[string]bool {
	if len(exts) == 0 {
		return nil
	}
	m := make(map[string]bool, len(exts))
	for _, e := range exts {
		if e = strings.ToLower(strings.TrimPrefix(e, ".")); e != "" {
			m[e] = true
		}
	}
	return m
}

// extractIssuePathRefs pulls concrete file references out of an issue
// body: tokens that are either a path (slash-separated segments ending
// in a known source extension, like `config/rbac/role.yaml`) or a bare
// filename (`AGENTS.md`). Commands (`make manifests`), RBAC groups
// (`core/endpoints`), marker annotations (`kubebuilder:rbac`), and API
// type paths (`discovery.k8s.io/v1.EndpointSlice`) do not qualify.
// Escaped backticks (as returned by the GitHub API) are normalized
// away before tokenizing. Results are deduplicated in first-occurrence
// order.
//
// sourceExtensions is the task's declared GateProfile source language
// (e.g. [".gd"] for Godot); those extensions are unioned with the
// language-agnostic pathRefExtensions so the extractor sees the repo's
// primary source files, not only Go and docs.
func extractIssuePathRefs(body string, sourceExtensions []string) []string {
	if body == "" {
		return nil
	}
	extraExts := extensionSet(sourceExtensions)
	normalized := strings.ReplaceAll(body, "\\`", "`")
	normalized = strings.NewReplacer("`", " ", "(", " ", ")", " ", "[", " ", "]", " ",
		"{", " ", "}", " ", "\"", " ", "'", " ", ",", " ", ";", " ").Replace(normalized)

	var refs []string
	seen := map[string]bool{}
	for _, tok := range strings.Fields(normalized) {
		tok = strings.Trim(tok, ".:!?")
		// Strip the line citation before the extension lookup, and keep the
		// bare path: refs are matched against the diff by exact path or
		// basename, neither of which a ":135" suffix would survive. Dedup
		// runs on the stripped form so repeated cites of the same file at
		// different lines collapse to one ref.
		tok = lineSuffixPattern.ReplaceAllString(tok, "")
		if tok == "" || seen[tok] {
			continue
		}
		if isPathRef(tok, extraExts) {
			seen[tok] = true
			refs = append(refs, tok)
		}
	}
	return dedupRefs(refs)
}

// dedupRefs collapses refs that resolve to the same file so the count of
// issue-named files is not inflated (#1447). A bare filename that is a path
// suffix of a fuller ref (automation-sync.ts vs src/lib/automation-sync.ts)
// names the same module and must not be counted as two distinct files; the
// more specific (longer) ref is kept. Refs that are not suffix-related (e.g.
// a/foo.ts and b/foo.ts) are distinct and are all preserved.
func dedupRefs(refs []string) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		replaced := false
		for i, k := range out {
			if samePathRef(k, r) {
				// Prefer the more specific (longer) of the two.
				if len(r) > len(k) {
					out[i] = r
				}
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, r)
		}
	}
	return out
}

// samePathRef reports whether two refs resolve to the same file: either they
// are equal, or one is a path suffix of the other aligned on a path boundary
// (a bare filename that ends a fuller path, or vice versa). The "/" boundary
// guard keeps, e.g., x-automation-sync.ts from matching automation-sync.ts.
func samePathRef(a, b string) bool {
	if a == b {
		return true
	}
	full, short := a, b
	if len(full) < len(short) {
		full, short = short, full
	}
	return len(short) < len(full) && strings.HasSuffix(full, "/"+short)
}

// hasSourceFile reports whether any path in paths ends with one of the
// extensions in exts. If exts is empty, it defaults to [".go"] so
// existing behavior is preserved.
func hasSourceFile(paths []string, exts []string) bool {
	if len(exts) == 0 {
		exts = []string{".go"}
	}
	for _, p := range paths {
		for _, ext := range exts {
			if strings.HasSuffix(p, ext) {
				return true
			}
		}
	}
	return false
}

// isPathRef reports whether a single cleaned token is a concrete file
// reference per the rules documented on extractIssuePathRefs. extraExts
// carries the task's declared source extensions (dotless-lowercase, via
// extensionSet) so a repo's primary language is recognized alongside the
// language-agnostic pathRefExtensions base.
func isPathRef(tok string, extraExts map[string]bool) bool {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(tok), "."))
	if !pathRefExtensions[ext] && !extraExts[ext] && !bareSourceFilenames[path.Base(tok)] {
		return false
	}
	for _, seg := range strings.Split(tok, "/") {
		if seg == "" || strings.IndexFunc(seg, func(r rune) bool {
			return !(r == '.' || r == '_' || r == '-' ||
				(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
		}) != -1 {
			return false
		}
	}
	return true
}

// joinDir recombines a path.Dir result with a basename without emitting a
// redundant "/." for bare filenames.
func joinDir(dir, base string) string {
	if dir == "." || dir == "" {
		return base
	}
	return dir + "/" + base
}

// testTargetsForPath returns the module path(s) a diff path could be testing.
// A diff that creates or edits a test file counts as touching the module
// under test (#1447): an "add tests for X" issue names X.ts, the correct
// change creates X.test.ts, and without this mapping the scope-overlap rail
// reports the diff touches none of the named files and the issue is
// unwinnable.
//
// The directory is preserved; only the basename is mapped. Recognized
// conventions:
//   - X.test.{ts,tsx,js,jsx} -> X.{ts,tsx,js,jsx}
//   - X.spec.{ts,tsx,js,jsx} -> X.{ts,tsx,js,jsx}
//   - X_test.go              -> X.go
//   - test_X.py              -> X.py
//   - X_test.py              -> X.py
//
// Returns nil when the path is not a test file under any of those
// conventions.
func testTargetsForPath(p string) []string {
	base := path.Base(p)
	ext := path.Ext(base)
	if ext == "" {
		return nil
	}
	stem := strings.TrimSuffix(base, ext)
	dir := path.Dir(p)

	// JS/TS: X.test.ts / X.spec.ts -> X.ts (extension preserved).
	for _, marker := range []string{".test", ".spec"} {
		if strings.HasSuffix(stem, marker) {
			module := strings.TrimSuffix(stem, marker) + ext
			return []string{joinDir(dir, module)}
		}
	}
	// Go: X_test.go -> X.go.
	if ext == ".go" && strings.HasSuffix(stem, "_test") {
		module := strings.TrimSuffix(stem, "_test") + ext
		return []string{joinDir(dir, module)}
	}
	// Python: test_X.py -> X.py and X_test.py -> X.py.
	if ext == ".py" {
		var modules []string
		if strings.HasPrefix(stem, "test_") {
			modules = append(modules, joinDir(dir, strings.TrimPrefix(stem, "test_")+ext))
		}
		if strings.HasSuffix(stem, "_test") {
			modules = append(modules, joinDir(dir, strings.TrimSuffix(stem, "_test")+ext))
		}
		return modules
	}
	return nil
}

// contentProbe pairs a test file's workspace-relative diff path with the
// content to check: the file's whole body when the branch ADDED it, or only
// the lines the branch added when it merely MODIFIED it.
type contentProbe struct {
	path    string
	content string
}

// testContentReferencesModule reports whether any probe's content references
// modulePath. It is the content-based half of the #1610 vouch: name-based
// folding (testTargetsForPath / testTargetsWithLayout) folds the test file's
// NAME to its subject, which fails for a test named after a feature or
// behaviour. This instead checks the test's content and asks whether it
// imports the module under test (contentReferencesModule), the deterministic,
// model-free signal that a feature-named test really does cover the module.
//
// The probe content is chosen for soundness, not cost. A test file this branch
// ADDED is checked on its whole body: a new test's imports cannot predate the
// work. A test file this branch merely MODIFIED is checked on its added LINES
// ONLY (#1616): the file may have imported the module long before the branch
// touched it, so vouching on its whole content would let a coder earn a scope
// pass by editing any test that happens to import the named module, without
// adding coverage for it. The added lines are the sound discriminator — a test
// that gains `from pkg.module import ...` in this diff is evidence; one that
// merely already contained it is not. This now covers the append-to-an-existing
// test-file shape (misospace/pr-reviewer-action#438) the #1614 slice left out.
//
// A probe whose content is empty (an unreadable added file, a modified file
// with no added lines) is simply not a vouch: a read failure must never flip a
// ref from unmatched to matched, so the rail degrades to name-based behaviour
// rather than inventing coverage it cannot verify.
func testContentReferencesModule(probes []contentProbe, modulePath string) bool {
	for _, p := range probes {
		if !isTestFile(p.path) {
			continue
		}
		if contentReferencesModule(p.content, modulePath) {
			return true
		}
	}
	return false
}

// buildContentProbes assembles the content probes for the vouch. diff-ADDED
// test files are probed on their whole body (readFile); diff-MODIFIED test
// files are probed on their added lines only (readAddedLines). A nil readFile
// disables the added-file probes and a nil readAddedLines disables the
// modified-file probes, so passing nil for both reproduces the pre-#1616
// name-only behaviour exactly.
func buildContentProbes(
	diffFiles, addedFiles []string,
	readFile func(relPath string) ([]byte, error),
	readAddedLines func(relPath string) (string, error),
) []contentProbe {
	added := make(map[string]bool, len(addedFiles))
	for _, f := range addedFiles {
		added[f] = true
	}
	var probes []contentProbe
	if readFile != nil {
		for _, f := range addedFiles {
			if !isTestFile(f) {
				continue
			}
			if content, err := readFile(f); err == nil {
				probes = append(probes, contentProbe{path: f, content: string(content)})
			}
		}
	}
	if readAddedLines != nil {
		for _, f := range diffFiles {
			if !isTestFile(f) || added[f] {
				continue
			}
			if lines, err := readAddedLines(f); err == nil {
				probes = append(probes, contentProbe{path: f, content: lines})
			}
		}
	}
	return probes
}

// isTestFile reports whether p is a test file under any of the recognized
// conventions. The content vouch only reads test files: a source file importing
// a module is not evidence a test covers it, so restricting to test files keeps
// the vouch scoped to test-coverage issues. It delegates to stripTestDecoration
// (the broadest recognizer, covering the beside-the-code conventions plus the
// parallel-tree forms like foo_spec.rb and FooTest.java) rather than
// testTargetsForPath, so a feature-named test in a parallel tree is still read
// for its content even when name-folding would not have folded it.
func isTestFile(p string) bool {
	return stripTestDecoration(path.Base(p)) != ""
}

// enforceReviewerScopeOverlap is the computable half of scope-drift
// detection (#647). The issue body names concrete files; the ground-truth
// diff says which files actually changed. When the issue names at least
// one file and the diff touches none of them, that is the #379 drift
// signature, and it is detectable without any model judgment, which
// matters because review judgment is demonstrably stochastic: the same
// devstral that caught #379's drift in May approved it in June.
//
// Policy mirrors enforceReviewerIssueAsk:
//   - refs exist, none matched, verdict GO: demote to NO-GO so the
//     workload controller's escalation emission routes the branch to a
//     bigger reviewer.
//   - refs exist, none matched, other verdict: annotate only.
//   - no refs in the issue, or no diff available: observe-only, no
//     annotations (absence of signal is not evidence of drift).
//
// Matching is generous on purpose (exact path or basename equality):
// a false drift flag costs one escalation review, while a missed match
// would demote a legitimate branch.
// addedFiles carries the subset of the branch's diff that was ADDED (git
// status "A"), readFile resolves an ADDED workspace-relative diff path to its
// bytes, and readAddedLines resolves a MODIFIED workspace-relative diff path
// to the lines the branch added to it (via repo.DiffAddedLines). All three
// are only used by the content-based vouch (#1610, extended to modified files
// in #1616): a test file named for a feature (test_dedup.py) defeats name-based
// folding, but its body — or, for a modified file, the lines this branch added
// to it — imports the module it covers, and reading that import is the
// deterministic signal name-folding cannot express. Pass nil for all three to
// disable the vouch and keep the pure name-based behaviour (every existing
// name-only test does).
func enforceReviewerScopeOverlap(
	log logr.Logger,
	extra map[string]any,
	issueBody string,
	diffFiles []string,
	verdict foremanv1alpha1.AgenticTaskVerdict,
	sourceExtensions []string,
	testLayout TestLayout,
	addedFiles []string,
	readFile func(relPath string) ([]byte, error),
	readAddedLines func(relPath string) (string, error),
) foremanv1alpha1.AgenticTaskVerdict {
	if extra == nil {
		return verdict
	}
	// Each short-circuit below is a case where the rail cannot answer, not one
	// where it answered "no drift". Record which, so a verdict produced without
	// the check is distinguishable afterwards from one that earned it (#1605).
	// Returning the model's verdict silently is what let this go unseen.
	if issueBody == "" {
		recordRailSkipped(extra, railScopeOverlap, skipReasonNoIssueBody)
		return verdict
	}
	if len(diffFiles) == 0 {
		recordRailSkipped(extra, railScopeOverlap, skipReasonNoDiffFiles)
		return verdict
	}
	refs := extractIssuePathRefs(issueBody, sourceExtensions)
	if len(refs) == 0 {
		// The issue named nothing checkable, so the rail cannot answer. Record
		// it like the other short-circuits: a GO that passed through here did
		// not earn a scope vouch, and nothing else says so.
		recordRailSkipped(extra, railScopeOverlap, skipReasonNoPathRefs)
		return verdict
	}

	diffBases := make(map[string]bool, len(diffFiles))
	diffPaths := make(map[string]bool, len(diffFiles))
	for _, f := range diffFiles {
		diffPaths[f] = true
		diffBases[path.Base(f)] = true
		// A test file also counts as touching the module it tests (#1447).
		// Index the mapped target under both full path and basename so a
		// ref that names either the module's directory or just its file
		// matches the test file that covers it.
		for _, t := range testTargetsWithLayout(f, testLayout) {
			diffPaths[t] = true
			diffBases[path.Base(t)] = true
		}
	}
	matched := []string{}
	matchedByName := map[string]bool{}
	for _, r := range refs {
		// A ref is satisfied when the diff touches the module it names
		// directly, or when the diff touches a test file that targets it:
		// testTargetsForPath has folded each test file's subject into
		// diffPaths/diffBases, so an "add tests for X" issue (naming X)
		// matches the X.test file the diff creates (#1447).
		if diffPaths[r] && diffBases[path.Base(r)] {
			matched = append(matched, r)
			matchedByName[r] = true
		}
	}

	// Content-based vouch (#1610, extended to modified files in #1616):
	// name-based folding fails for a test file named for a feature or
	// behaviour (test_dedup.py, test_platform_gh_api_forgejo.py) because its
	// name does not derive from the module it covers. For each ref the name
	// fold left unmatched, fall back to checking the test content probes: an
	// ADDED test file probed on its whole body, or a MODIFIED test file probed
	// on its added lines, vouches when it imports the module it covers.
	// Ordering is load-bearing — the content check runs only on the refs name
	// matching left unmatched, so a path match is never re-derived from
	// content, and the two signals stay distinguishable in the record.
	probes := buildContentProbes(diffFiles, addedFiles, readFile, readAddedLines)
	if len(matched) == 0 && len(probes) > 0 {
		for _, r := range refs {
			if matchedByName[r] {
				continue
			}
			if testContentReferencesModule(probes, r) {
				matched = append(matched, r)
				matchedByName[r] = true
				if extra[scopeMatchedViaContentKey] == nil {
					extra[scopeMatchedViaContentKey] = []string{r}
				} else {
					extra[scopeMatchedViaContentKey] = append(extra[scopeMatchedViaContentKey].([]string), r)
				}
			}
		}
	}

	drift := len(matched) < len(refs)
	extra["scopeRefs"] = refs
	extra["scopeMatched"] = matched
	extra["scopeDriftDetected"] = drift
	if !drift {
		return verdict
	}

	// A diff with zero indexable source files is not scope drift — it is a
	// legitimate docs- or YAML-only change (#800). Skip the scope check
	// so the run proceeds.
	if !hasSourceFile(diffFiles, sourceExtensions) {
		extra["scopeDriftNotDemoted"] = scopeNotDemotedNoSourceFile
		return verdict
	}

	if verdict != foremanv1alpha1.AgenticTaskVerdictGo {
		extra["scopeDriftNotDemoted"] = scopeNotDemotedAlreadyNonGo
		log.Info("reviewer scope: diff touches none of the files the issue names; verdict already non-GO, annotating only",
			"verdict", verdict, "scopeRefs", refs)
		return verdict
	}
	// Named alongside the flag so a consumer can tell this demotion apart
	// from the issueAsk rail's (#1636): scope drift is a statement about the
	// diff, which a fix iteration can act on.
	extra["verdictDemoted"] = true
	extra["verdictDemotedBy"] = railScopeOverlap
	// First-writer-wins for the claimed verdict: the scope rail runs before
	// the issueAsk rail, so once it archives the reviewer's original verdict
	// the second rail must not overwrite it with the already-demoted value
	// (#1678). The archived value has to be the pre-demotion verdict for
	// inertDemotion to tell a real GO->NO-GO rewrite apart from a rail that
	// merely re-annotated.
	if _, ok := extra["verdictClaimed"]; !ok {
		extra["verdictClaimed"] = string(foremanv1alpha1.AgenticTaskVerdictNoGo)
	}
	extra["demotionReason"] = fmt.Sprintf(
		"scope drift: the issue names %d file(s) (%s) and the diff touches none of them",
		len(refs), strings.Join(refs, ", "))
	log.Info("reviewer scope: drift detected on GO verdict; demoting to NO-GO",
		"scopeRefs", refs, "diffFiles", diffFiles)
	return foremanv1alpha1.AgenticTaskVerdictNoGo
}
