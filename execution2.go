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

func buildBoundaryQueryDocuments(ctx context.Context, schema *ast.Schema, step QueryPlanStep, ids []string, parentTypeBoundaryField BoundaryQuery) ([]string, error) {
	if parentTypeBoundaryField.Array {
		selectionSetQL := formatSelectionSetSingleLine(ctx, schema, step.SelectionSet)
		qids := []string{}
		for _, id := range ids {
			qids = append(qids, fmt.Sprintf("%q", id))
		}
		idsQL := fmt.Sprintf("[%s]", strings.Join(qids, ", "))
		return []string{fmt.Sprintf(`{ %s(ids: %s) %s }`, parentTypeBoundaryField.Query, idsQL, selectionSetQL)}, nil
	}
	panic("not implemented")
}
