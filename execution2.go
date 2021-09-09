package bramble

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// FIXME: dedupe result?
func extractBoundaryIDs(data interface{}, insertionPoint []string) ([]string, error) {
	ptr := data
	if len(insertionPoint) == 0 {
		switch ptr := ptr.(type) {
		case map[string]interface{}:
			value, ok := ptr["_id"]
			if !ok {
				return nil, errors.New("extractBoundaryIDs: unexpected missing '_id' in map")
			}
			str, ok := value.(string)
			if !ok {
				return nil, errors.New("extractBoundaryIDs: unexpected non string '_id' in map")
			}
			return []string{str}, nil
		case []interface{}:
			result := []string{}
			for innerPtr := range ptr {
				ids, err := extractBoundaryIDs(innerPtr, insertionPoint)
				if err != nil {
					return nil, err
				}
				result = append(result, ids...)
			}
			return result, nil
		default:
			return nil, fmt.Errorf("extractBoundaryIDs: unexpected type: %T", ptr)
		}
	}
	switch ptr := ptr.(type) {
	case map[string]interface{}:
		if len(insertionPoint) == 1 {
			return extractBoundaryIDs(ptr[insertionPoint[0]], nil)
		} else {
			return extractBoundaryIDs(ptr[insertionPoint[0]], insertionPoint[1:])
		}
	case []interface{}:
		result := []string{}
		for _, innerPtr := range ptr {
			ids, err := extractBoundaryIDs(innerPtr, insertionPoint)
			if err != nil {
				return nil, err
			}
			result = append(result, ids...)
		}
		return result, nil
	default:
		return nil, fmt.Errorf("extractBoundaryIDs: unexpected type: %T", ptr)
	}
}

func buildBoundaryQueryDocuments(ctx context.Context, schema *ast.Schema, step QueryPlanStep, ids []string, parentTypeBoundaryField BoundaryQuery, batchSize int) ([]string, error) {
	selectionSetQL := formatSelectionSetSingleLine(ctx, schema, step.SelectionSet)
	if parentTypeBoundaryField.Array {
		qids := []string{}
		for _, id := range ids {
			qids = append(qids, fmt.Sprintf("%q", id))
		}
		idsQL := fmt.Sprintf("[%s]", strings.Join(qids, ", "))
		return []string{fmt.Sprintf(`{ _result: %s(ids: %s) %s }`, parentTypeBoundaryField.Query, idsQL, selectionSetQL)}, nil
	}

	var (
		documents      []string
		selectionIndex int
	)
	for _, batch := range batchBy(ids, batchSize) {
		var selections []string
		for _, id := range batch {
			selection := fmt.Sprintf("%s: %s(id: %q) %s", nodeAlias(selectionIndex), parentTypeBoundaryField.Query, id, selectionSetQL)
			selections = append(selections, selection)
			selectionIndex++
		}
		document := "{ " + strings.Join(selections, " ") + " }"
		documents = append(documents, document)
	}

	return documents, nil
}

func batchBy(items []string, batchSize int) (batches [][]string) {
	for batchSize < len(items) {
		items, batches = items[batchSize:], append(batches, items[0:batchSize:batchSize])
	}

	return append(batches, items)
}

type ExecutionResult struct {
	ServiceURL     string
	InsertionPoint []string
	Data           map[string]interface{}
}

func mergeExecutionResults(results []ExecutionResult) (map[string]interface{}, error) {
	if len(results) == 0 {
		return nil, errors.New("mergeExecutionResults: nothing to merge")
	}
	if len(results) == 1 {
		return results[0].Data, nil
	}
	data := results[0].Data
	for _, result := range results[1:] {
		if err := mergeExecutionResultsRec(result.Data, data, result.InsertionPoint); err != nil {
			return nil, err
		}
	}
	return data, nil
}

func mergeExecutionResultsRec(src map[string]interface{}, dst interface{}, insertionPoint []string) error {
	// base case for root steps (insertion point is always empty for root steps)
	if len(insertionPoint) == 0 {
		switch ptr := dst.(type) {
		case map[string]interface{}:
			for k, v := range src {
				ptr[k] = v
			}
		default:
			return fmt.Errorf("mergeExecutionResultsRec: unxpected type '%T' for top-level merge", ptr)
		}
		return nil
	}
	// base case for child steps (insertion point is never empty for child steps)
	if len(insertionPoint) == 1 {
		switch ptr := dst.(type) {
		case map[string]interface{}:
			ptr, err := mapAtJSONPath(ptr, insertionPoint[0])
			if err != nil {
				return err
			}
			data, err := mapAtJSONPath(src, nodeAlias(0))
			if err != nil {
				return err
			}
			for k, v := range data {
				ptr[k] = v
			}
		case []interface{}:
			for _, dstValue := range ptr {
				dstID, err := valueAtJSONPath(dstValue, insertionPoint[0], "_id")
				if err != nil {
					return err
				}
				srcValues, err := getBoundaryFieldResults(src)
				if err != nil {
					return err
				}
				for _, srcValue := range srcValues {
					srcID, err := valueAtJSONPath(srcValue, "_id")
					if err != nil {
						return err
					}
					if srcID == dstID {
						srcMap, err := mapAtJSONPath(srcValue)
						if err != nil {
							return err
						}
						for k, v := range srcMap {
							dstMap, err := mapAtJSONPath(dstValue, insertionPoint[0])
							if err != nil {
								return err
							}
							dstMap[k] = v
						}
					}
				}
			}
		default:
			return fmt.Errorf("mergeExecutionResultsRec: unxpected type '%T' for non top-level merge", ptr)
		}
		return nil
	}
	// recursive case
	switch ptr := dst.(type) {
	case map[string]interface{}:
		if err := mergeExecutionResultsRec(src, ptr[insertionPoint[0]], insertionPoint[1:]); err != nil {
			return err
		}
	case []interface{}:
		for _, innerPtr := range ptr {
			if err := mergeExecutionResultsRec(src, innerPtr, insertionPoint); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("mergeExecutionResultsRec: unxpected type '%T' for non top-level merge", ptr)
	}
	return nil
}

func mapAtJSONPath(value interface{}, path ...string) (map[string]interface{}, error) {
	result, err := valueAtJSONPath(value, path...)
	if err != nil {
		return nil, err
	}
	resultMap, ok := result.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("mapAtJSONPath: expected value to be a 'map[string]interface{}' but got '%T'", result)
	}
	return resultMap, nil
}

func valueAtJSONPath(val interface{}, path ...string) (interface{}, error) {
	for len(path) > 0 {
		valMap, ok := val.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("valueAtJSONPath: expected value to be a 'map[string]interface{}' but got '%T'", val)
		}
		val, ok = valMap[path[0]]
		if !ok {
			return nil, errors.New("valueAtJSONPath: invalid path")
		}
		path = path[1:]
	}
	return val, nil
}

func getBoundaryFieldResults(src map[string]interface{}) ([]map[string]interface{}, error) {
	arrayBoundaryFieldResults, ok := src["_result"]
	if ok {
		slice, ok := arrayBoundaryFieldResults.([]interface{})
		if !ok {
			return nil, fmt.Errorf("getBoundaryFieldResults: expected value to be a '[]map[string]interface{}' but got '%T'", arrayBoundaryFieldResults)
		}
		var result []map[string]interface{}
		for i, element := range slice {
			elementMap, ok := element.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("getBoundaryFieldResults: expect value at index %d to be map[string]interface{}' but got '%T'", i, element)
			}
			result = append(result, elementMap)
		}
		return result, nil
	}
	var result []map[string]interface{}
	for i := 0; i < len(src); i++ {
		element, ok := src[nodeAlias(i)]
		if !ok {
			return nil, fmt.Errorf("getBoundaryFieldResults: unexpected missing key '%s'", nodeAlias(i))
		}
		elementMap, ok := element.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("getBoundaryFieldResults: expect value at '%s' to be map[string]interface{}' but got '%T'", nodeAlias(i), element)
		}
		result = append(result, elementMap)
	}
	return result, nil
}

var ErrNullBubbledToRoot = errors.New("bubbleUpNullValuesInPlace: null bubbled up to root")

// bubbleUpNullValuesInPlace checks for expected null values (as per schema) and bubbles them up if needed, and checks for
// unexpected null values and returns errors for each (these unexpected nulls are also bubbled up).
// See https://spec.graphql.org/June2018/#sec-Errors-and-Non-Nullability
func bubbleUpNullValuesInPlace(schema *ast.Schema, selectionSet ast.SelectionSet, result map[string]interface{}) (GraphqlErrors, error) {
	errs, bubbleUp, err := bubbleUpNullValuesInPlaceRec(schema, nil, selectionSet, result, ast.Path{})
	if err != nil {
		return nil, err
	}
	if bubbleUp {
		return nil, ErrNullBubbledToRoot
	}
	return errs, nil
}

func bubbleUpNullValuesInPlaceRec(schema *ast.Schema, currentType *ast.Type, selectionSet ast.SelectionSet, result interface{}, path ast.Path) (errs GraphqlErrors, bubbleUp bool, err error) {
	switch result := result.(type) {
	case map[string]interface{}:
		for _, selection := range selectionSet {
			switch selection := selection.(type) {
			case *ast.Field:
				field := selection
				value := result[field.Alias]
				if field.SelectionSet != nil {
					lowerErrs, lowerBubbleUp, lowerErr := bubbleUpNullValuesInPlaceRec(schema, field.Definition.Type, field.SelectionSet, value, append(path, ast.PathName(field.Alias)))
					if lowerErr != nil {
						return nil, false, lowerErr
					}
					if lowerBubbleUp {
						if field.Definition.Type.NonNull {
							bubbleUp = true
						} else {
							result[field.Alias] = nil
						}
					}
					errs = append(errs, lowerErrs...)
				} else {
					if value == nil && field.Definition.Type.NonNull {
						errs = append(errs, GraphqlError{Message: "TODO", Path: append(path, ast.PathName(field.Alias)), Extensions: nil})
						bubbleUp = true
					}
				}
			case *ast.FragmentSpread:
				fragment := selection
				// FIXME: deal with type when inside interface
				lowerErrs, lowerBubbleUp, lowerErr := bubbleUpNullValuesInPlaceRec(schema, currentType, fragment.Definition.SelectionSet, result, path)
				if lowerErr != nil {
					return nil, false, lowerErr
				}
				bubbleUp = lowerBubbleUp
				errs = append(errs, lowerErrs...)
			default:
				err = fmt.Errorf("unknown selection type: %T", selection)
				return
			}
		}
	case []interface{}:
		for i, value := range result {
			lowerErrs, lowerBubbleUp, lowerErr := bubbleUpNullValuesInPlaceRec(schema, currentType, selectionSet, value, append(path, ast.PathIndex(i)))
			if lowerErr != nil {
				return nil, false, lowerErr
			}
			if lowerBubbleUp {
				if currentType.Elem.NonNull {
					bubbleUp = true
				} else {
					result[i] = nil
				}
			}
			errs = append(errs, lowerErrs...)
		}
	default:
		return nil, false, fmt.Errorf("bubbleUpNullValuesInPlaceRec: unxpected result type '%T'", result)
	}
	return
}