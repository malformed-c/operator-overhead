package bench

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ═══════════════════════════════════════════════════════════════════════════
// SIZE IS THREE DIFFERENT QUESTIONS AND THIS FILE REFUSES TO ANSWER THEM WITH
// ONE NUMBER.
//
//	the ARTIFACT   what an author builds and hands over
//	the IMAGE      what the node library holds and moves between pawns
//	the SOURCE     what a person writes and another person reviews
//
// They do not scale together and the arm that wins depends on which one is
// meant, so a single "size" column would let a reader pick the flattering one
// without noticing they had.
//
// ***THE SHIPPED-AT-N TABLE EXISTS TO SHOW THAT NEITHER ARM MULTIPLIES.*** Both
// ship ONE image at every N: arm A's instance is a `-id` flag, arm B's is
// `spec.config`. An artifact parameterised at runtime is what keeps size out of
// the scaling question, and a reader has to be able to see that rather than take
// it on trust — so the table is printed even though both rows are flat.
// ═══════════════════════════════════════════════════════════════════════════

// Size is what one arm costs to ship.
type Size struct {
	Arm string `json:"arm"`

	// Artifact is what the build produces, on disk, uncompressed.
	Artifact      string `json:"artifact"`
	ArtifactBytes int64  `json:"artifactBytes"`

	// Image is the same thing after `apsis ingest`, as `apsis images` reports
	// it in `size_bytes`.
	//
	// ***IT IS LARGER THAN THE ARTIFACT AND I CANNOT SAY WHY, SO THIS FIELD IS
	// NAMED AFTER ITS SOURCE RATHER THAN AFTER WHAT IT MEANS.*** Measured: a
	// 30.79 MiB binary ingests to 40.42 MiB. Layers are stored extracted under
	// `/var/lib/apsis/perigeos/layers` — visible from the neighbouring layers —
	// but the layer directory itself is root-only, so the accounting was NOT
	// verified from the files.
	//
	// It is therefore not a compressed transfer size, not necessarily an
	// on-disk size, and must not be quoted as either. It is the library's own
	// figure, comparable BETWEEN arms because both were ingested the same way,
	// and that comparability is the only thing it is used for here.
	ImageRef   string `json:"imageRef"`
	ImageBytes int64  `json:"imageBytes"`

	// PerInstance is how many DISTINCT artifacts N instances need, and how an
	// instance is told which pair it serves. One image for both arms now; the
	// only difference is whether the parameter arrives as a flag or as config.
	PerInstance string `json:"perInstance"`

	// Source is the program a person writes.
	Source SourceCount `json:"source"`
}

// SourceCount separates the three kinds of line, because "lines of code" quoted
// over a heavily-commented file measures the commenting.
type SourceCount struct {
	Path    string `json:"path"`
	Code    int    `json:"code"`
	Comment int    `json:"comment"`
	Blank   int    `json:"blank"`
}

// ShippedAt is the total bytes an arm puts in the node library at one N.
type ShippedAt struct {
	N     int              `json:"n"`
	Bytes map[string]int64 `json:"bytes"`
}

// SizeReport is the whole static comparison.
type SizeReport struct {
	Sizes   []Size      `json:"sizes"`
	Shipped []ShippedAt `json:"shipped"`
	// Crossover is the smallest N in the ladder at which the Perseid arm has
	// put more bytes in the library than the controller-runtime arm. Zero when
	// it never does within the ladder.
	Crossover int `json:"crossover"`
}

// MeasureSizes builds the static comparison from artifacts on disk and the
// node's image library.
//
// It measures rather than asserts: every byte count here comes from `stat` or
// from `apsis images --json`, and the source counts come from reading the files.
// Nothing in this report is typed in.
func MeasureSizes(ctx context.Context, apsisBin, crrelayPath, componentPath string, ladder []int) (SizeReport, error) {
	images, err := imageSizes(ctx, apsisBin)
	if err != nil {
		return SizeReport{}, err
	}

	cr := Size{
		Arm:         "controller-runtime (A1, A2)",
		Artifact:    crrelayPath,
		ImageRef:    "crrelay:v1",
		PerInstance: "1 image, shared by all N (the instance is a -id flag)",
	}
	perseid := Size{
		Arm:         "perseid (B)",
		Artifact:    componentPath,
		ImageRef:    ComponentRef(),
		PerInstance: "1 image, shared by all N (the instance is spec.config)",
	}

	for _, s := range []*Size{&cr, &perseid} {
		if fi, err := os.Stat(s.Artifact); err == nil {
			s.ArtifactBytes = fi.Size()
		} else {
			return SizeReport{}, fmt.Errorf("bench: stat %s: %w — build it first, because a "+
				"missing artifact reported as zero bytes is the smallest possible lie", s.Artifact, err)
		}
		s.ImageBytes = images[s.ImageRef]
	}

	cr.Source, err = CountSource("cmd/crrelay/main.go")
	if err != nil {
		return SizeReport{}, err
	}
	perseid.Source, err = CountSource("perseid/relay/src/main.ts")
	if err != nil {
		return SizeReport{}, err
	}

	rep := SizeReport{Sizes: []Size{cr, perseid}}
	for _, n := range ladder {
		// ***BOTH ARMS SHIP ONE IMAGE AT EVERY N.*** Neither multiplies, because
		// both parameterise one artifact at runtime rather than building per
		// instance. The column is here to show that, not to carry a slope.
		rep.Shipped = append(rep.Shipped, ShippedAt{N: n, Bytes: map[string]int64{
			cr.Arm:      cr.ImageBytes,
			perseid.Arm: perseid.ImageBytes,
		}})
		if rep.Crossover == 0 && perseid.ImageBytes > cr.ImageBytes {
			rep.Crossover = n
		}
	}

	return rep, nil
}

// imageSizes reads the node library's own accounting.
func imageSizes(ctx context.Context, apsisBin string) (map[string]int64, error) {
	cmd := exec.CommandContext(ctx, apsisBin, "images", "--json")
	cmd.Stderr = os.Stderr
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("bench: %s images --json: %w", apsisBin, err)
	}
	var report struct {
		Images []struct {
			Name      string `json:"name"`
			SizeBytes int64  `json:"size_bytes"`
		} `json:"images"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil, fmt.Errorf("bench: parse image list: %w", err)
	}
	out := make(map[string]int64, len(report.Images))
	for _, i := range report.Images {
		out[i.Name] = i.SizeBytes
	}

	return out, nil
}

// CountSource classifies every line of a Go or TypeScript file.
//
// ***COMMENTS ARE COUNTED SEPARATELY BECAUSE BOTH FILES ARE MOSTLY COMMENTS,
// AND "LINES OF CODE" OVER EITHER WOULD MEASURE THE COMMENTING.*** The claim
// under test is ADR-0075's — that an operator should be a program you write
// rather than a product you deploy — and it is a claim about the program, not
// about how much its author explained themselves.
//
// Deliberately simple: `//` and `/* … */`, which both languages share. It does
// not know that a `//` inside a string literal is not a comment. That
// mis-classifies at most a handful of lines in these two files, in the same
// direction for both, and a real parser for two languages is a lot of machinery
// to make a footnote more exact.
func CountSource(path string) (SourceCount, error) {
	f, err := os.Open(path)
	if err != nil {
		return SourceCount{}, fmt.Errorf("bench: count %s: %w", path, err)
	}
	defer f.Close()

	out := SourceCount{Path: path}
	inBlock := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case inBlock:
			out.Comment++
			if strings.Contains(line, "*/") {
				inBlock = false
			}
		case line == "":
			out.Blank++
		case strings.HasPrefix(line, "//"):
			out.Comment++
		case strings.HasPrefix(line, "/*"):
			out.Comment++
			inBlock = !strings.Contains(line, "*/")
		default:
			out.Code++
		}
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("bench: read %s: %w", path, err)
	}

	return out, nil
}
