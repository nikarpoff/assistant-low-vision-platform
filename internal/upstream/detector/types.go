package detector

// Direction is the side of the frame the object is on (DET-4).
type Direction string

// Directions.
const (
	DirectionLeft  Direction = "left"
	DirectionAhead Direction = "ahead"
	DirectionRight Direction = "right"
)

// Distance is the rough distance to the object (DET-4).
type Distance string

// Distances.
const (
	DistanceNear Distance = "near"
	DistanceFar  Distance = "far"
)

// DetectResponse is the body of POST /v1/detect (DET-1).
type DetectResponse struct {
	Image   ImageSize          `json:"image"`
	Objects []Object           `json:"objects"` // sorted by area, the largest first
	Model   Model              `json:"model"`
	Timings map[string]float64 `json:"timings"` // stage -> milliseconds
}

// ImageSize is the size of the decoded snapshot in pixels.
type ImageSize struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Object is one detected object.
type Object struct {
	Label      string    `json:"label"` // class id of the vocabulary
	LabelRU    string    `json:"label_ru"`
	Confidence float64   `json:"confidence"`
	Box        Box       `json:"box"`
	Direction  Direction `json:"direction"`
	Distance   Distance  `json:"distance"`
	DistanceM  *float64  `json:"distance_m"` // reserved for a depth estimate, null for now
	Area       float64   `json:"area"`       // share of the frame area
	Key        bool      `json:"key"`
}

// Box is the bounding box normalized to [0, 1]: x1 < x2, y1 < y2.
type Box struct {
	X1 float64 `json:"x1"`
	Y1 float64 `json:"y1"`
	X2 float64 `json:"x2"`
	Y2 float64 `json:"y2"`
}

// Model is the model that answered.
type Model struct {
	Backend string `json:"backend"`
	Name    string `json:"name"`
	Imgsz   int    `json:"imgsz"`
	Device  string `json:"device"`
}
