package utils

import (
	"errors"
	"reflect"
	"strings"

	"github.com/go-playground/validator/v10"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
)

const errValidation = "validation failed"

// ValidatorResponse is the result of a validation: Message is safe to return to the client.
type ValidatorResponse struct {
	Error   error
	Message string
}

// Validator validates request structs by their `validate` tags.
type Validator struct {
	baseValidator *validator.Validate
}

// NewValidator creates a new Validator.
func NewValidator() *Validator {
	return &Validator{
		baseValidator: validator.New(),
	}
}

// WithJsonFieldNames makes validation messages use json field names instead of Go ones.
func (v *Validator) WithJsonFieldNames() *Validator {
	v.baseValidator.RegisterTagNameFunc(func(field reflect.StructField) string {
		return strings.SplitN(field.Tag.Get("json"), ",", 2)[0]
	})

	return v
}

// Validate checks the object and describes the failed fields.
func (v *Validator) Validate(object any) ValidatorResponse {
	if err := v.baseValidator.Struct(object); err != nil {
		logger.Error(err)

		return ValidatorResponse{
			Error:   err,
			Message: processValidationError(err),
		}
	}

	return ValidatorResponse{}
}

func processValidationError(err error) string {
	var target validator.ValidationErrors
	if !errors.As(err, &target) {
		return errValidation
	}

	fields := make([]string, 0, len(target))
	for _, e := range target {
		fields = append(fields, e.Field())
	}

	return errValidation + " for fields: " + strings.Join(fields, ", ")
}
