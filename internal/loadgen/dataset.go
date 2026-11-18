package loadgen

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Files of a data set (EXP-1).
const (
	annotationsFile = "annotations.json"
	questionsFile   = "questions.jsonl"
)

// Audio of the questions: PCM s16le mono 16 kHz, as AUDIO_IN (PROTO-3).
const (
	audioSampleRate    = 16000
	audioChannels      = 1
	audioBitsPerSample = 16
	audioBytesPerMS    = audioSampleRate * audioChannels * audioBitsPerSample / 8 / 1000
	wavFormatPCM       = 1
	wavHeaderSize      = 12
	wavChunkHeaderSize = 8
)

// Texts of the errors, shared by the code and the tests.
const (
	errReadFileFormat      = "failed to read %q: %w"
	errParseFileFormat     = "failed to parse %q: %w"
	errParseLineFormat     = "failed to parse %q line %d: %w"
	errNoImages            = "data set has no images"
	errImageFieldsFormat   = "image %d: file_name, width and height are required"
	errImageDuplicateForm  = "image %d is listed twice"
	errNotJPEGFormat       = "%q is not a JPEG file"
	errQuestionFieldsForm  = "question on line %d: id and audio are required"
	errQuestionImageFormat = "question %q refers to unknown image %d"
	errNotWAVFormat        = "%q is not a RIFF WAVE file"
	errWAVFormatFormat     = "%q must be PCM s16le mono 16 kHz"
	errWAVNoDataFormat     = "%q has no audio data"
)

// Dataset is a data set of EXP-1 as loadgen runs it: the snapshots and the questions with their audio.
type Dataset struct {
	Images    []Image
	Questions []Question
}

// Image is a snapshot of annotations.json with its JPEG.
type Image struct {
	ID       int    `json:"id"`
	FileName string `json:"file_name"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Scene    string `json:"scene"`

	Data []byte `json:"-"`
}

// Question is a line of questions.jsonl with its audio.
type Question struct {
	ID        string `json:"id"`
	Audio     string `json:"audio"`
	Text      string `json:"text"`
	Condition string `json:"condition"`
	ImageID   *int   `json:"image_id,omitempty"`

	PCM []byte `json:"-"` // PCM s16le mono 16 kHz without the WAV header
}

// AudioMS returns the duration of the question audio, ms.
func (q *Question) AudioMS() float64 {
	return float64(len(q.PCM)) / audioBytesPerMS
}

// annotations is the part of the COCO file loadgen needs.
type annotations struct {
	Images []Image `json:"images"`
}

// LoadDataset reads a data set of EXP-1 from dir: annotations.json with the snapshots and
// questions.jsonl with the questions. The question manifest is optional.
func LoadDataset(dir string) (*Dataset, error) {
	images, err := loadImages(dir)
	if err != nil {
		return nil, err
	}

	byID := make(map[int]bool, len(images))
	for _, image := range images {
		byID[image.ID] = true
	}
	questions, err := loadQuestions(dir, byID)
	if err != nil {
		return nil, err
	}

	return &Dataset{Images: images, Questions: questions}, nil
}

// Image returns the snapshot with the id.
func (d *Dataset) Image(id int) *Image {
	for i := range d.Images {
		if d.Images[i].ID == id {
			return &d.Images[i]
		}
	}
	return nil
}

func loadImages(dir string) ([]Image, error) {
	path := filepath.Join(dir, annotationsFile)
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf(errReadFileFormat, path, err)
	}
	var doc annotations
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf(errParseFileFormat, path, err)
	}
	if len(doc.Images) == 0 {
		return nil, errors.New(errNoImages)
	}

	seen := make(map[int]bool, len(doc.Images))
	for i := range doc.Images {
		image := &doc.Images[i]
		if image.FileName == "" || image.Width <= 0 || image.Height <= 0 {
			return nil, fmt.Errorf(errImageFieldsFormat, image.ID)
		}
		if seen[image.ID] {
			return nil, fmt.Errorf(errImageDuplicateForm, image.ID)
		}
		seen[image.ID] = true

		imagePath := filepath.Join(dir, image.FileName)
		if image.Data, err = os.ReadFile(imagePath); err != nil {
			return nil, fmt.Errorf(errReadFileFormat, imagePath, err)
		}
		if !bytes.HasPrefix(image.Data, []byte{0xFF, 0xD8, 0xFF}) {
			return nil, fmt.Errorf(errNotJPEGFormat, imagePath)
		}
	}
	return doc.Images, nil
}

func loadQuestions(dir string, images map[int]bool) ([]Question, error) {
	path := filepath.Join(dir, questionsFile)
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf(errReadFileFormat, path, err)
	}
	defer file.Close()

	questions := []Question{}
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var question Question
		if err := json.Unmarshal([]byte(text), &question); err != nil {
			return nil, fmt.Errorf(errParseLineFormat, path, line, err)
		}
		if question.ID == "" || question.Audio == "" {
			return nil, fmt.Errorf(errQuestionFieldsForm, line)
		}
		if question.ImageID != nil && !images[*question.ImageID] {
			return nil, fmt.Errorf(errQuestionImageFormat, question.ID, *question.ImageID)
		}
		if question.PCM, err = readWAV(filepath.Join(dir, question.Audio)); err != nil {
			return nil, err
		}
		questions = append(questions, question)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf(errReadFileFormat, path, err)
	}
	return questions, nil
}

// readWAV returns the samples of a WAV file PCM s16le mono 16 kHz.
func readWAV(path string) ([]byte, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf(errReadFileFormat, path, err)
	}
	if len(body) < wavHeaderSize || string(body[0:4]) != "RIFF" || string(body[8:12]) != "WAVE" {
		return nil, fmt.Errorf(errNotWAVFormat, path)
	}

	formatOK := false
	for offset := wavHeaderSize; offset+wavChunkHeaderSize <= len(body); {
		id := string(body[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(body[offset+4 : offset+8]))
		start := offset + wavChunkHeaderSize
		end := min(start+size, len(body))

		switch id {
		case "fmt ":
			if end-start < 16 {
				return nil, fmt.Errorf(errWAVFormatFormat, path)
			}
			chunk := body[start:end]
			formatOK = binary.LittleEndian.Uint16(chunk[0:2]) == wavFormatPCM &&
				binary.LittleEndian.Uint16(chunk[2:4]) == audioChannels &&
				binary.LittleEndian.Uint32(chunk[4:8]) == audioSampleRate &&
				binary.LittleEndian.Uint16(chunk[14:16]) == audioBitsPerSample
			if !formatOK {
				return nil, fmt.Errorf(errWAVFormatFormat, path)
			}
		case "data":
			if !formatOK {
				return nil, fmt.Errorf(errWAVFormatFormat, path)
			}
			if end == start {
				return nil, fmt.Errorf(errWAVNoDataFormat, path)
			}
			return body[start:end], nil
		}
		offset = start + size + size%2 // chunks are padded to an even size
	}
	return nil, fmt.Errorf(errWAVNoDataFormat, path)
}
