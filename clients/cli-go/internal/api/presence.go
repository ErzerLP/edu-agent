package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

var (
	timeType       = reflect.TypeFor[time.Time]()
	rawMessageType = reflect.TypeFor[json.RawMessage]()
)

func validateRequiredJSONFields(data []byte, target any) error {
	targetType := reflect.TypeOf(target)
	if targetType == nil || targetType.Kind() != reflect.Pointer {
		return errors.New("presence target must be a pointer")
	}
	return validateJSONPresence(json.RawMessage(data), targetType.Elem(), "response", false)
}

func validateJSONPresence(raw json.RawMessage, valueType reflect.Type, path string, nullable bool) error {
	for valueType.Kind() == reflect.Pointer {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil
		}
		valueType = valueType.Elem()
	}
	if valueType == timeType || valueType == rawMessageType || valueType.Kind() == reflect.Interface {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if nullable {
			return nil
		}
		return fmt.Errorf("%s must not be null", path)
	}
	switch valueType.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return fmt.Errorf("%s must be an object", path)
		}
		for index := 0; index < valueType.NumField(); index++ {
			field := valueType.Field(index)
			if !field.IsExported() {
				continue
			}
			name, optional := jsonField(field)
			fieldNullable := nullableJSONField(valueType, field.Name)
			if name == "-" {
				continue
			}
			fieldRaw, present := object[name]
			if !present {
				if optional {
					continue
				}
				return fmt.Errorf("%s.%s is required", path, name)
			}
			if err := validateJSONPresence(fieldRaw, field.Type, path+"."+name, fieldNullable); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return fmt.Errorf("%s must be an array", path)
		}
		for index, item := range items {
			if err := validateJSONPresence(item, valueType.Elem(), fmt.Sprintf("%s[%d]", path, index), false); err != nil {
				return err
			}
		}
	case reflect.Map:
		var entries map[string]json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			return fmt.Errorf("%s must be an object", path)
		}
		for key, entry := range entries {
			if err := validateJSONPresence(entry, valueType.Elem(), path+"."+key, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func jsonField(field reflect.StructField) (string, bool) {
	tag := field.Tag.Get("json")
	if tag == "" {
		return field.Name, false
	}
	parts := strings.Split(tag, ",")
	name := parts[0]
	if name == "" {
		name = field.Name
	}
	for _, option := range parts[1:] {
		if option == "omitempty" {
			return name, true
		}
	}
	return name, false
}

func nullableJSONField(valueType reflect.Type, fieldName string) bool {
	switch valueType.Name() {
	case "KnowledgeMaintenanceIdentityImpact":
		switch fieldName {
		case "PreservedDocumentIDs", "AddedDocumentIDs", "RemovedDocumentIDs", "MovedDocumentIDs", "PreservedNodeIDs", "AddedNodeIDs", "RemovedNodeIDs":
			return true
		}
	case "KnowledgeMaintenanceLineageImpact":
		return fieldName == "Lineages"
	case "KnowledgeMaintenanceEvidenceImpact":
		return fieldName == "References"
	case "KnowledgeMaintenanceRisk":
		return fieldName == "Reasons"
	}
	return false
}
