package auth

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/credentialweight"
)

// EffectiveAuthWeight returns the same effective weight used by credential selection.
// Attributes.weight takes precedence over Metadata.weight; absent weight defaults to 1,
// while malformed or non-positive explicit values return 0.
func EffectiveAuthWeight(auth *Auth) int64 {
	if auth == nil {
		return credentialweight.Default
	}
	if rawWeight, ok := auth.Attributes[AttributeWeight]; ok && strings.TrimSpace(rawWeight) != "" {
		weight, errParse := credentialweight.ParseString(rawWeight)
		if errParse != nil {
			return 0
		}
		return weight
	}
	if rawWeight, ok := auth.Metadata[AttributeWeight]; ok {
		weight, errParse := credentialweight.ParseValue(rawWeight)
		if errParse != nil {
			return 0
		}
		return weight
	}
	return credentialweight.Default
}

// ValidateAuthWeight validates every explicit credential weight source.
func ValidateAuthWeight(auth *Auth) error {
	if auth == nil {
		return nil
	}
	if rawWeight, ok := auth.Attributes[AttributeWeight]; ok {
		if _, errParse := credentialweight.ParseString(rawWeight); errParse != nil {
			return fmt.Errorf("invalid attributes weight: %w", errParse)
		}
	}
	if rawWeight, ok := auth.Metadata[AttributeWeight]; ok {
		if _, errParse := credentialweight.ParseValue(rawWeight); errParse != nil {
			return fmt.Errorf("invalid metadata weight: %w", errParse)
		}
	}
	return nil
}

// ApplyAuthWeightMetadata validates the auth and applies a source metadata weight.
func ApplyAuthWeightMetadata(auth *Auth, metadata map[string]any) error {
	if errWeight := ValidateAuthWeight(auth); errWeight != nil {
		return errWeight
	}
	if auth == nil || metadata == nil {
		return nil
	}
	rawWeight, ok := metadata[AttributeWeight]
	if !ok {
		return nil
	}
	weight, errParse := credentialweight.ParseValue(rawWeight)
	if errParse != nil {
		return fmt.Errorf("invalid metadata weight: %w", errParse)
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes[AttributeWeight] = strconv.FormatInt(weight, 10)
	return nil
}
