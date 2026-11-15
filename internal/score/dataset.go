package score

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/vocabulary"
)

// Files of a data set (EXP-1).
const (
	annotationsFile  = "annotations.json"
	descriptionsFile = "descriptions.jsonl"
)

// Texts of the dataset errors, shared by the code and the tests.
const (
	errReadFileFormat         = "failed to read %q: %w"
	errParseFileFormat        = "failed to parse %q: %w"
	errParseLineFormat        = "failed to parse %q line %d: %w"
	errNoDescriptionsFormat   = "%q has no descriptions"
	errDescriptionTwiceFormat = "image %d is described twice in %q"
	errUnknownClassFormat     = "image %d: class %q is not in the vocabulary"
	errUnknownDirectionFormat = "image %d: unknown direction %q of class %q: allowed values are left, ahead, right"
	errUnknownCategoryFormat  = "%q: annotation %d refers to unknown category %d"
)

// coco is the part of annotations.json the scoring needs.
type coco struct {
	Images []struct {
		ID    int    `json:"id"`
		Scene string `json:"scene"`
	} `json:"images"`
	Categories []struct {
		ID   int    `json:"id"`
		Name string `json:"name"` // class id of the vocabulary (EXP-1)
	} `json:"categories"`
	Annotations []struct {
		ID         int `json:"id"`
		ImageID    int `json:"image_id"`
		CategoryID int `json:"category_id"`
	} `json:"annotations"`
}

// LoadDataset reads the annotation of a data set of EXP-1 from dir: descriptions.jsonl with the
// key objects (required) and annotations.json with the scenes and all the annotated objects
// (optional: without it the classes of a snapshot are its key objects). Classes and directions
// are checked against the vocabulary.
func LoadDataset(dir string, vocab *vocabulary.Vocabulary) (map[int]*Image, error) {
	images, err := loadDescriptions(filepath.Join(dir, descriptionsFile), vocab)
	if err != nil {
		return nil, err
	}
	if err := loadAnnotations(filepath.Join(dir, annotationsFile), images); err != nil {
		return nil, err
	}
	return images, nil
}

// loadDescriptions reads the key objects of the snapshots.
func loadDescriptions(path string, vocab *vocabulary.Vocabulary) (map[int]*Image, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf(errReadFileFormat, path, err)
	}
	defer file.Close()

	images := map[int]*Image{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var description Description
		if err := json.Unmarshal([]byte(text), &description); err != nil {
			return nil, fmt.Errorf(errParseLineFormat, path, line, err)
		}
		if _, ok := images[description.ImageID]; ok {
			return nil, fmt.Errorf(errDescriptionTwiceFormat, description.ImageID, path)
		}

		image := &Image{ID: description.ImageID, Classes: map[string]bool{}}
		for _, object := range description.KeyObjects {
			if !vocab.Has(object.Class) {
				return nil, fmt.Errorf(errUnknownClassFormat, image.ID, object.Class)
			}
			if _, ok := directionPatterns[object.Direction]; !ok {
				return nil, fmt.Errorf(errUnknownDirectionFormat, image.ID, object.Direction, object.Class)
			}
			image.KeyObjects = append(image.KeyObjects, object)
			image.Classes[object.Class] = true
		}
		images[image.ID] = image
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf(errReadFileFormat, path, err)
	}
	if len(images) == 0 {
		return nil, fmt.Errorf(errNoDescriptionsFormat, path)
	}
	return images, nil
}

// loadAnnotations adds the scenes and the annotated classes of the snapshots.
func loadAnnotations(path string, images map[int]*Image) error {
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf(errReadFileFormat, path, err)
	}
	var doc coco
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf(errParseFileFormat, path, err)
	}

	for _, entry := range doc.Images {
		if image, ok := images[entry.ID]; ok {
			image.Scene = entry.Scene
		}
	}
	categories := make(map[int]string, len(doc.Categories))
	for _, category := range doc.Categories {
		categories[category.ID] = category.Name
	}
	for _, annotation := range doc.Annotations {
		class, ok := categories[annotation.CategoryID]
		if !ok {
			return fmt.Errorf(errUnknownCategoryFormat, path, annotation.ID, annotation.CategoryID)
		}
		if image, ok := images[annotation.ImageID]; ok {
			image.Classes[class] = true
		}
	}
	return nil
}

// directionPatterns are the words of the directions in the normalized text (PLT-4):
// the words of the template (DSC-4) and their usual synonyms.
var directionPatterns = map[detector.Direction]string{
	detector.DirectionLeft:  `слева|налево|левее|по левую руку|с левой стороны`,
	detector.DirectionAhead: `впереди|спереди|прямо|перед вами|перед тобой`,
	detector.DirectionRight: `справа|направо|правее|по правую руку|с правой стороны`,
}
