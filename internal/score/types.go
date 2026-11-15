// Package score scores the answer texts of loadgen against the annotation of the data set
// (PLT-19, EXP-4) and gives the verdict on the hypothesis (EXP-8).
//
// The metrics of EXP-4:
//   - key_objects_with_direction - the key objects named with the class and the right
//     direction in the same sentence, divided by all key objects (micro average);
//   - key_objects_class_only - the same without the direction: only the class is mentioned;
//   - hallucinated - the mentions of key classes absent from the annotation of the snapshot,
//     divided by all mentions of key classes.
//
// Mentions are found by the patterns of the detector vocabulary (DET-2) in the text
// normalized by PLT-4.
package score

import (
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
)

// Limits of the hypothesis (EXP-8). The comparisons are integer and the limits are included.
const (
	// E2EP95LimitMS is the limit of p95 «touch → sound» of describe, ms.
	E2EP95LimitMS = 2000
	// KeyObjectsLimitPercent is the lower limit of key_objects_with_direction, %.
	KeyObjectsLimitPercent = 75
	// LLMFallbackShareLimitPercent is the upper limit of llm_fallback_share, %.
	LLMFallbackShareLimitPercent = 10
	// MinRuns is the number of runs the verdict needs (EXP-3, EXP-8).
	MinRuns = 3
)

// Verdicts of the hypothesis (EXP-8).
const (
	VerdictConfirmed        = "confirmed"
	VerdictNotConfirmed     = "not_confirmed"
	VerdictInsufficientData = "insufficient_data"
)

// Result is the JSON of cmd/score.
type Result struct {
	ConfigIDs  []string      `json:"config_ids"` // every config_id of the JSONL, in order of appearance
	Files      []string      `json:"files"`
	Runs       []RunTotals   `json:"runs"`
	Pooled     Totals        `json:"pooled"`
	Spread     Spread        `json:"spread"`
	Images     []ImageTotals `json:"images"`
	Hypothesis *Hypothesis   `json:"hypothesis,omitempty"` // only with --stats
}

// Totals are the figures of a run, of the union of the runs or of a snapshot.
type Totals struct {
	Answers                 int      `json:"answers"`    // scored describe answers
	Unanswered              int      `json:"unanswered"` // describe requests without answer.end, not scored
	KeyObjects              int      `json:"key_objects"`
	NamedWithDirection      int      `json:"named_with_direction"`
	NamedClassOnly          int      `json:"named_class_only"`
	Mentions                int      `json:"mentions"` // mentions of key classes
	HallucinatedMentions    int      `json:"hallucinated_mentions"`
	KeyObjectsWithDirection *float64 `json:"key_objects_with_direction"` // null without key objects
	KeyObjectsClassOnly     *float64 `json:"key_objects_class_only"`     // null without key objects
	Hallucinated            *float64 `json:"hallucinated"`               // null without mentions
}

// RunTotals are the figures of one run.
type RunTotals struct {
	Run int `json:"run"`
	Totals
}

// ImageTotals are the figures of one snapshot over all the runs.
type ImageTotals struct {
	ImageID int    `json:"image_id"`
	Scene   string `json:"scene"`
	Totals
}

// Spread is the spread of the shares between the runs.
type Spread struct {
	KeyObjectsWithDirection *Range `json:"key_objects_with_direction"`
	KeyObjectsClassOnly     *Range `json:"key_objects_class_only"`
	Hallucinated            *Range `json:"hallucinated"`
}

// Range is the minimum, the maximum and the mean of a share over the runs; the runs without
// the share are not counted, and with no such run the range is null.
type Range struct {
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
}

// Hypothesis is the verdict of EXP-8; a missing value and its check are null.
type Hypothesis struct {
	ConfigID                *string  `json:"config_id"`
	Runs                    int      `json:"runs"`
	E2EP95MS                *float64 `json:"e2e_p95_ms"`
	E2EP95LimitMS           int      `json:"e2e_p95_limit_ms"`
	KeyObjectsWithDirection *float64 `json:"key_objects_with_direction"`
	KeyObjectsLimit         float64  `json:"key_objects_limit"`
	LLMFallbackShare        *float64 `json:"llm_fallback_share"`
	LLMFallbackShareLimit   float64  `json:"llm_fallback_share_limit"`
	Checks                  Checks   `json:"checks"`
	Verdict                 string   `json:"verdict"`
}

// Checks are the results of the three conditions of EXP-8.
type Checks struct {
	E2E       *bool `json:"e2e"`
	Quality   *bool `json:"quality"`
	Fallbacks *bool `json:"fallbacks"`
}

// KeyObject is a key object of a snapshot: a class of the vocabulary and its direction.
type KeyObject struct {
	Class     string             `json:"class"`
	Direction detector.Direction `json:"direction"`
}

// Description is a line of descriptions.jsonl (EXP-1).
type Description struct {
	ImageID    int         `json:"image_id"`
	KeyObjects []KeyObject `json:"key_objects"`
	Reference  string      `json:"reference"`
}

// Image is the annotation of a snapshot as the scoring needs it.
type Image struct {
	ID         int
	Scene      string
	KeyObjects []KeyObject
	Classes    map[string]bool // classes of the annotation and of the key objects
}
