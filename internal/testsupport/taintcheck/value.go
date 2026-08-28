package taintcheck

import "reflect"

type valueVisit struct {
	kind     reflect.Kind
	typeName reflect.Type
	pointer  uintptr
	length   int
	capacity int
}

type valueScanner struct {
	canaries []string
	seen     map[valueVisit]struct{}
}

// ScanValue recursively scans exported structured values without invoking
// formatting or marshaling methods. Pointer, map, and slice identities are
// tracked so cyclic values terminate. Unexported struct fields are skipped.
func ScanValue(value any, canaries []string) (result Result) {
	result.Complete = true
	defer func() {
		if recover() != nil {
			result = Result{}
		}
	}()
	scanner := valueScanner{canaries: canaries, seen: make(map[valueVisit]struct{})}
	return scanner.scan(reflect.ValueOf(value))
}

func (scanner *valueScanner) scan(value reflect.Value) Result {
	if !value.IsValid() {
		return Result{Complete: true}
	}
	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			return Result{Complete: true}
		}
		return scanner.scan(value.Elem())
	}

	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() || scanner.visited(value, 0, 0) {
			return Result{Complete: true}
		}
		return scanner.scan(value.Elem())
	case reflect.Map:
		if value.IsNil() || scanner.visited(value, value.Len(), 0) {
			return Result{Complete: true}
		}
		iterator := value.MapRange()
		for iterator.Next() {
			if result := scanner.scan(iterator.Key()); result.Found || !result.Complete {
				return result
			}
			if result := scanner.scan(iterator.Value()); result.Found || !result.Complete {
				return result
			}
		}
	case reflect.Slice:
		if value.IsNil() {
			return Result{Complete: true}
		}
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return Scan(value.Bytes(), scanner.canaries)
		}
		if scanner.visited(value, value.Len(), value.Cap()) {
			return Result{Complete: true}
		}
		for index := 0; index < value.Len(); index++ {
			if result := scanner.scan(value.Index(index)); result.Found || !result.Complete {
				return result
			}
		}
	case reflect.Array:
		for index := 0; index < value.Len(); index++ {
			if result := scanner.scan(value.Index(index)); result.Found || !result.Complete {
				return result
			}
		}
	case reflect.Struct:
		valueType := value.Type()
		for index := 0; index < value.NumField(); index++ {
			if valueType.Field(index).PkgPath != "" {
				continue
			}
			if result := scanner.scan(value.Field(index)); result.Found || !result.Complete {
				return result
			}
		}
	case reflect.String:
		return Scan([]byte(value.String()), scanner.canaries)
	}
	return Result{Complete: true}
}

func (scanner *valueScanner) visited(value reflect.Value, length, capacity int) bool {
	visit := valueVisit{
		kind: value.Kind(), typeName: value.Type(), pointer: value.Pointer(),
		length: length, capacity: capacity,
	}
	if _, present := scanner.seen[visit]; present {
		return true
	}
	scanner.seen[visit] = struct{}{}
	return false
}
