package bramble

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"golang.org/x/sync/errgroup"
)

var (
	errNilBoundaryData   = errors.New("found a null when attempting to extract boundary ids")
	errNullBubbledToRoot = errors.New("bubbleUpNullValuesInPlace: null bubbled up to root")
)

type ExecutionResult struct {
	ServiceURL     string
	InsertionPoint []string
	Data           interface{}
	Errors         gqlerror.List
}

type QueryExecution struct {
	schema       *ast.Schema
	requestCount int32

	maxRequest    int32
	graphqlClient *GraphQLClient
	// FIXME: rename the entire type
	boundaryQueries BoundaryQueriesMap
}

func newQueryExecution(client *GraphQLClient, schema *ast.Schema, boundaryQueries BoundaryQueriesMap, maxRequest int32) *QueryExecution {
	return &QueryExecution{
		schema:          schema,
		graphqlClient:   client,
		boundaryQueries: boundaryQueries,
		maxRequest:      maxRequest,
	}
}

func (q *QueryExecution) Execute(ctx context.Context, queryPlan QueryPlan) ([]ExecutionResult, gqlerror.List) {
	readWg := &sync.WaitGroup{}
	group, ctx := errgroup.WithContext(ctx)
	resultsChan := make(chan ExecutionResult)
	results := []ExecutionResult{}

	if len(queryPlan.RootSteps) > int(q.maxRequest) {
		return nil, gqlerror.List{&gqlerror.Error{
			Message: fmt.Sprintf("exceeded max requests of %v", q.maxRequest),
		}}
	}

	for _, step := range queryPlan.RootSteps {
		if step.ServiceURL == internalServiceName {
			r, err := ExecuteBrambleStep(step)
			if err != nil {
				return nil, q.createGQLErrors(ctx, *step, err)
			}
			results = append(results, *r)
			continue
		}

		step := step
		group.Go(func() error {
			return q.ExecuteRootStep(ctx, *step, resultsChan, group)
		})
	}

	readWg.Add(1)
	go func() {
		for result := range resultsChan {
			results = append(results, result)
		}
		readWg.Done()
	}()

	if err := group.Wait(); err != nil {
		return nil, gqlerror.List{&gqlerror.Error{
			Message: err.Error(),
		}}
	}
	close(resultsChan)
	readWg.Wait()
	return results, nil
}

func (q *QueryExecution) ExecuteRootStep(ctx context.Context, step QueryPlanStep, resultsChan chan ExecutionResult, group *errgroup.Group) error {
	var document string
	if step.ParentType == "Query" {
		document = "query " + formatSelectionSet(ctx, q.schema, step.SelectionSet)
	} else if step.ParentType == "Mutation" {
		document = "mutation " + formatSelectionSet(ctx, q.schema, step.SelectionSet)
	} else {
		return errors.New("non mutation or query root step")
	}

	var data map[string]interface{}

	err := q.executeDocument(ctx, document, step.ServiceURL, &data)
	if err != nil {
		resultsChan <- ExecutionResult{
			ServiceURL:     step.ServiceURL,
			InsertionPoint: step.InsertionPoint,
			Errors:         q.createGQLErrors(ctx, step, err),
		}
		return nil
	}

	resultsChan <- ExecutionResult{
		ServiceURL:     step.ServiceURL,
		InsertionPoint: step.InsertionPoint,
		Data:           data,
	}

	for _, childStep := range step.Then {
		boundaryIDs, err := extractBoundaryIDs(data, childStep.InsertionPoint)
		if err == errNilBoundaryData {
			continue
		}
		if err != nil {
			return err
		}

		childStep := childStep

		group.Go(func() error {
			return q.executeChildStep(ctx, *childStep, boundaryIDs, resultsChan, group)
		})
	}
	return nil
}

func (q *QueryExecution) executeChildStep(ctx context.Context, step QueryPlanStep, boundaryIDs []string, resultsChan chan ExecutionResult, group *errgroup.Group) error {
	atomic.AddInt32(&q.requestCount, 1)
	if q.requestCount > q.maxRequest {
		return fmt.Errorf("exceeded max requests of %v", q.maxRequest)
	}

	boundaryFieldGetter := q.boundaryQueries.Query(step.ServiceURL, step.ParentType)

	documents, err := buildBoundaryQueryDocuments(ctx, q.schema, step, boundaryIDs, boundaryFieldGetter, 50)
	if err != nil {
		return err
	}

	data, err := q.executeBoundaryQuery(ctx, documents, step.ServiceURL, boundaryFieldGetter)
	if err != nil {
		resultsChan <- ExecutionResult{
			ServiceURL:     step.ServiceURL,
			InsertionPoint: step.InsertionPoint,
			Data:           data,
			Errors:         q.createGQLErrors(ctx, step, err),
		}
		return nil
	}

	resultsChan <- ExecutionResult{
		ServiceURL:     step.ServiceURL,
		InsertionPoint: step.InsertionPoint,
		Data:           data,
	}

	for _, childStep := range step.Then {
		boundaryIDs, err := extractBoundaryIDs(data, childStep.InsertionPoint[1:]) // FIXME: validate this always holds true
		if err == errNilBoundaryData {
			continue
		}
		if err != nil {
			return err
		}
		childStep := childStep
		group.Go(func() error {
			return q.executeChildStep(ctx, *childStep, boundaryIDs, resultsChan, group)
		})
	}

	return nil
}

func (e *QueryExecution) createGQLErrors(ctx context.Context, step QueryPlanStep, err error) gqlerror.List {
	var path ast.Path
	for _, p := range step.InsertionPoint {
		path = append(path, ast.PathName(p))
	}

	var locs []gqlerror.Location
	for _, f := range selectionSetToFields(step.SelectionSet) {
		pos := f.GetPosition()
		if pos == nil {
			continue
		}
		locs = append(locs, gqlerror.Location{Line: pos.Line, Column: pos.Column})

		// if the field has a subset it's part of the path
		if len(f.SelectionSet) > 0 {
			path = append(path, ast.PathName(f.Alias))
		}
	}

	var gqlErr GraphqlErrors
	var outputErrs gqlerror.List
	if errors.As(err, &gqlErr) {
		for _, ge := range gqlErr {
			extensions := ge.Extensions
			if extensions == nil {
				extensions = make(map[string]interface{})
			}
			extensions["selectionSet"] = formatSelectionSetSingleLine(ctx, e.schema, step.SelectionSet)
			extensions["serviceName"] = step.ServiceName
			extensions["serviceUrl"] = step.ServiceURL

			outputErrs = append(outputErrs, &gqlerror.Error{
				Message:    ge.Message,
				Path:       path,
				Locations:  locs,
				Extensions: extensions,
			})
		}
		return outputErrs
	} else {
		outputErrs = append(outputErrs, &gqlerror.Error{
			Message:   err.Error(),
			Path:      path,
			Locations: locs,
			Extensions: map[string]interface{}{
				"selectionSet": formatSelectionSetSingleLine(ctx, e.schema, step.SelectionSet),
			},
		})
	}

	return outputErrs
}

func (q *QueryExecution) executeBoundaryQuery(ctx context.Context, documents []string, serviceURL string, boundaryFieldGetter BoundaryQuery) ([]interface{}, error) {
	output := make([]interface{}, 0, 0)
	if !boundaryFieldGetter.Array {
		for _, document := range documents {
			partialData := make(map[string]interface{})
			err := q.executeDocument(ctx, document, serviceURL, &partialData)
			if err != nil {
				return nil, err
			}
			for _, value := range partialData {
				output = append(output, value)
			}
		}
		return output, nil
	}

	if len(documents) != 1 {
		return nil, errors.New("there should only be a single document for array boundary field lookups")
	}

	data := struct {
		Result []interface{} `json:"_result"`
	}{}

	err := q.executeDocument(ctx, documents[0], serviceURL, &data)
	return data.Result, err
}

func (q *QueryExecution) executeDocument(ctx context.Context, document string, serviceURL string, response interface{}) error {
	req := NewRequest(document)
	req.Headers = GetOutgoingRequestHeadersFromContext(ctx)
	err := q.graphqlClient.Request(ctx, serviceURL, req, &response)
	return err
}

func ExecuteBrambleStep(queryPlanStep *QueryPlanStep) (*ExecutionResult, error) {
	result, err := BuildTypenameResponseMap(queryPlanStep.SelectionSet, queryPlanStep.ParentType)
	if err != nil {
		return nil, err
	}

	return &ExecutionResult{
		ServiceURL:     internalServiceName,
		InsertionPoint: []string{},
		Data:           result,
	}, nil
}

func BuildTypenameResponseMap(selectionSet ast.SelectionSet, parentTypeName string) (map[string]interface{}, error) {
	result := make(map[string]interface{})
	for _, field := range selectionSetToFields(selectionSet) {
		if field.SelectionSet != nil {
			if field.Definition.Type.NamedType == "" {
				return nil, fmt.Errorf("expected named type")
			}

			// FIXME: why are the definitions always null

			var err error
			result[field.Alias], err = BuildTypenameResponseMap(field.SelectionSet, field.Definition.Type.Name())
			if err != nil {
				return nil, err
			}
		} else {
			if field.Name != "__typename" {
				return nil, fmt.Errorf("expected __typename")
			}
			result[field.Alias] = parentTypeName
		}
	}
	return result, nil
}

func hasNamespaceDirective(directiveList ast.DirectiveList) bool {
	for _, directive := range directiveList {
		if directive.Name == "@namespace" {
			return true
		}
	}
	return false
}

// FIXME: dedupe result?
func extractBoundaryIDs(data interface{}, insertionPoint []string) ([]string, error) {
	ptr := data
	if ptr == nil {
		return nil, errNilBoundaryData
	}
	if len(insertionPoint) == 0 {
		switch ptr := ptr.(type) {
		case map[string]interface{}:
			var id string
			var ok bool
			id, ok = ptr["_id"].(string)
			if !ok {
				id, ok = ptr["id"].(string)
			}
			if !ok {
				return nil, errors.New("extractBoundaryIDs: unexpected missing '_id' or 'id' in map")
			}
			return []string{id}, nil
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

func mergeExecutionResults(results []ExecutionResult) (map[string]interface{}, error) {
	if len(results) == 0 {
		return nil, errors.New("mergeExecutionResults: nothing to merge")
	}

	if len(results) == 1 {
		data := results[0].Data
		if data == nil {
			return nil, nil
		}

		dataMap, ok := data.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("a complete graphql response should be map[string]interface{}, got %T", results[0].Data)
		}
		return dataMap, nil
	}

	data := results[0].Data
	for _, result := range results[1:] {
		if err := mergeExecutionResultsRec(result.Data, data, result.InsertionPoint); err != nil {
			return nil, err
		}
	}

	dataMap, ok := data.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("merged execution results should be map[string]interface{}, got %T", data)
	}

	return dataMap, nil
}

func mergeExecutionResultsRec(src interface{}, dst interface{}, insertionPoint []string) error {
	// base case
	if len(insertionPoint) == 0 {
		switch ptr := dst.(type) {
		case map[string]interface{}:
			switch src := src.(type) {
			// base case for root step merging
			case map[string]interface{}:
				mergeMaps(ptr, src)

			// base case for children step merging
			case []interface{}:
				boundaryResults, err := getBoundaryFieldResults(src)
				if err != nil {
					return err
				}

				dstID, err := boundaryIDFromMap(ptr)
				if err != nil {
					return err
				}

				for _, result := range boundaryResults {
					srcID, err := boundaryIDFromMap(result)
					if err != nil {
						return err
					}
					if srcID == dstID {
						for k, v := range result {
							if k == "_id" || k == "id" {
								continue
							}

							ptr[k] = v
						}
					}
				}

			}
		default:
			return fmt.Errorf("mergeExecutionResultsRec: unxpected type '%T' for top-level merge", ptr)
		}
		return nil
	}

	// recursive case
	switch ptr := dst.(type) {
	case map[string]interface{}:
		switch ptr := ptr[insertionPoint[0]].(type) {
		case []interface{}:
			for _, innerPtr := range ptr {
				if err := mergeExecutionResultsRec(src, innerPtr, insertionPoint[1:]); err != nil {
					return err
				}
			}
		default:
			if err := mergeExecutionResultsRec(src, ptr, insertionPoint[1:]); err != nil {
				return err
			}
		}
	case []interface{}:
		for _, innerPtr := range ptr {
			if err := mergeExecutionResultsRec(src, innerPtr, insertionPoint[1:]); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("mergeExecutionResultsRec: unxpected type '%T' for non top-level merge", ptr)
	}
	return nil
}

func boundaryIDFromMap(val interface{}) (string, error) {
	boundaryMap, ok := val.(map[string]interface{})
	if !ok {
		return "", errors.New("boundaryIDFromMap: expected map to extract boundary id")
	}
	id, ok := boundaryMap["_id"].(string)
	if ok {
		return id, nil
	}
	id, ok = boundaryMap["id"].(string)
	if ok {
		return id, nil
	}
	return "", errors.New("boundaryIDFromMap: 'id' or '_id' not found")
}

func getBoundaryFieldResults(src interface{}) ([]map[string]interface{}, error) {
	slice, ok := src.([]interface{})
	if !ok {
		return nil, fmt.Errorf("getBoundaryFieldResults: expected value to be a '[]map[string]interface{}' but got '%T'", slice)
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

// bubbleUpNullValuesInPlace checks for expected null values (as per schema) and bubbles them up if needed, and checks for
// unexpected null values and returns errors for each (these unexpected nulls are also bubbled up).
// See https://spec.graphql.org/June2018/#sec-Errors-and-Non-Nullability
func bubbleUpNullValuesInPlace(schema *ast.Schema, selectionSet ast.SelectionSet, result map[string]interface{}) (GraphqlErrors, error) {
	errs, bubbleUp, err := bubbleUpNullValuesInPlaceRec(schema, nil, selectionSet, result, ast.Path{})
	if err != nil {
		return nil, err
	}
	if bubbleUp {
		return nil, errNullBubbledToRoot
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
				if strings.HasPrefix(field.Name, "__") {
					continue
				}
				value := result[field.Alias]
				if value == nil {
					if field.Definition.Type.NonNull {
						errs = append(errs, GraphqlError{Message: "field failed to resolve", Path: append(path, ast.PathName(field.Alias)), Extensions: nil})
						bubbleUp = true
					}
					return
				}
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
				}
			case *ast.FragmentSpread:
				fragment := selection
				typename, ok := result["__typename"].(string)
				if !ok {
					return nil, false, errors.New("missing expected __typename")
				}
				if typename != fragment.Definition.TypeCondition && !implementsInterface(schema, typename, fragment.Definition.TypeCondition) {
					continue
				}
				lowerErrs, lowerBubbleUp, lowerErr := bubbleUpNullValuesInPlaceRec(schema, nil, fragment.Definition.SelectionSet, result, path)
				if lowerErr != nil {
					return nil, false, lowerErr
				}
				bubbleUp = lowerBubbleUp
				errs = append(errs, lowerErrs...)
			case *ast.InlineFragment:
				fragment := selection
				typename, ok := result["__typename"].(string)
				if !ok {
					return nil, false, errors.New("missing expected __typename")
				}
				if typename != fragment.TypeCondition && !implementsInterface(schema, typename, fragment.TypeCondition) {
					continue
				}
				lowerErrs, lowerBubbleUp, lowerErr := bubbleUpNullValuesInPlaceRec(schema, nil, fragment.SelectionSet, result, path)
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
	// FIXME: why does introspection cause this
	case []map[string]interface{}:
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

func formatResponseBody(schema *ast.Schema, selectionSet ast.SelectionSet, result map[string]interface{}) (string, error) {
	return formatResponseDataRec(schema, selectionSet, result, false)
}

// FIXME: better way to indicate we're inside a fragment than this bool
func formatResponseDataRec(schema *ast.Schema, selectionSet ast.SelectionSet, result interface{}, insideFragment bool) (string, error) {
	var buf bytes.Buffer
	if result == nil {
		return "null", nil
	}
	switch result := result.(type) {
	case map[string]interface{}:
		if !insideFragment {
			buf.WriteString("{")
		}
		for i, selection := range selectionSet {
			switch selection := selection.(type) {
			case *ast.InlineFragment:
				typename, ok := result["__typename"].(string)
				if !ok {
					return "", errors.New("expected typename")
				}

				currentTypeIsInterface := selection.ObjectDefinition.IsAbstractType()
				fragmentImplementsInterface := currentTypeIsInterface &&
					implementsInterface(schema, typename, selection.ObjectDefinition.Name)
				resultObjectTypeMatchesFragment := selection.TypeCondition == typename

				if fragmentImplementsInterface && !resultObjectTypeMatchesFragment {
					continue
				}

				innerBody, err := formatResponseDataRec(schema, selection.SelectionSet, result, true)
				if err != nil {
					return "", err
				}
				buf.WriteString(innerBody)

			case *ast.FragmentSpread:
				// FIXME: validate if we need to handle interface implementations like above, currently appears we do not
				innerBody, err := formatResponseDataRec(schema, selection.Definition.SelectionSet, result, true)
				if err != nil {
					return "", err
				}

				buf.WriteString(innerBody)
			case *ast.Field:
				field := selection
				fieldData, ok := result[field.Alias]
				// FIXME: should the bubbling not have taken care of this?
				if !ok && selection.Definition.Type.NonNull {
					return "", fmt.Errorf("got a null response for non-nullable field %q", field.Alias)
				} else if !ok {
					return "null", nil
				}
				buf.WriteString(fmt.Sprintf(`"%s":`, field.Alias))
				if field.SelectionSet != nil {
					innerBody, err := formatResponseDataRec(schema, field.SelectionSet, fieldData, false)
					if err != nil {
						return "", err
					}
					buf.WriteString(innerBody)
				} else {
					fieldJSON, err := json.Marshal(&fieldData)
					if err != nil {
						return "", err
					}
					buf.Write(fieldJSON)
				}

				if i < len(selectionSet)-1 {
					buf.WriteString(",")
				}
			}
		}
		if !insideFragment {
			buf.WriteString("}")
		}
	case []interface{}:
		buf.WriteString("[")
		for i, v := range result {
			innerBody, err := formatResponseDataRec(schema, selectionSet, v, false)
			if err != nil {
				return "", err
			}
			buf.WriteString(innerBody)

			if i < len(result)-1 {
				buf.WriteString(",")
			}
		}
		buf.WriteString("]")
	// FIXME: why does introspection cause this
	case []map[string]interface{}:
		buf.WriteString("[")
		for i, v := range result {
			innerBody, err := formatResponseDataRec(schema, selectionSet, v, false)
			if err != nil {
				return "", err
			}
			buf.WriteString(innerBody)

			if i < len(result)-1 {
				buf.WriteString(",")
			}
		}
		buf.WriteString("]")
	}

	return buf.String(), nil
}

func selectionFields(selectionSet ast.SelectionSet) []*ast.Field {
	var result []*ast.Field
	for _, selection := range selectionSet {
		switch selection := selection.(type) {
		case *ast.Field:
			result = append(result, selection)
		case *ast.FragmentSpread:
			definition := selection.Definition
			result = append(result, selectionFields(definition.SelectionSet)...)
		case *ast.InlineFragment:
			result = append(result, selectionFields(selection.SelectionSet)...)
		}
	}
	return result
}

func implementsInterface(schema *ast.Schema, objectType, interfaceType string) bool {
	for _, def := range schema.Implements[objectType] {
		if def.Name == interfaceType {
			return true
		}
	}
	return false
}
