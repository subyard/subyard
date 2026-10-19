package nativejson

import (
	"bytes"
	"encoding"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"reflect"
)

// EncodeLosslessNativeJSON encodes one concrete private native descriptor.
// Callers retain their own typed grammar and domain/capacity validation. Strings
// are byte columns so legal host paths never undergo UTF-8 replacement. Maps,
// interfaces and custom codecs cannot introduce a second executable grammar.
func EncodeLosslessNativeJSON(value any, limit int) ([]byte, error) {
	if limit <= 0 {
		return nil, errors.New("native JSON bound unavailable")
	}
	v := reflect.ValueOf(value)
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil, errors.New("native JSON root absent")
		}
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return nil, errors.New("native JSON requires a concrete descriptor")
	}
	encoded, err := nativeJSONStrings(v, true, 0)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(encoded.Interface())
	if err != nil {
		return nil, err
	}
	if len(raw) > limit {
		return nil, errors.New("native JSON capacity exceeded")
	}
	return raw, nil
}

func DecodeLosslessNativeJSON(raw []byte, target any, limit int) error {
	if limit <= 0 || len(raw) > limit {
		return errors.New("native JSON capacity exceeded")
	}
	pointer := reflect.ValueOf(target)
	if !pointer.IsValid() || pointer.Kind() != reflect.Pointer || pointer.IsNil() || pointer.Elem().Kind() != reflect.Struct {
		return errors.New("native JSON requires a concrete destination")
	}
	// Inspect the entire typed shape before JSON can invoke embedded codecs.
	if _, err := nativeJSONStrings(reflect.New(pointer.Elem().Type()).Elem(), true, 0); err != nil {
		return err
	}
	wire := reflect.New(pointer.Elem().Type())
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(wire.Interface()); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("native JSON trailing record")
	}
	decoded, err := nativeJSONStrings(wire.Elem(), false, 0)
	if err != nil {
		return err
	}
	canonical, err := EncodeLosslessNativeJSON(decoded.Interface(), limit)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, canonical) {
		return errors.New("native JSON is not canonical")
	}
	pointer.Elem().Set(decoded)
	return nil
}

var nativeJSONCodecTypes = []reflect.Type{
	reflect.TypeFor[json.Marshaler](), reflect.TypeFor[json.Unmarshaler](),
	reflect.TypeFor[encoding.TextMarshaler](), reflect.TypeFor[encoding.TextUnmarshaler](),
}

func nativeJSONStrings(v reflect.Value, encode bool, depth int) (reflect.Value, error) {
	if depth > 64 {
		return reflect.Value{}, errors.New("native JSON nesting exceeded")
	}
	t := v.Type()
	for _, codec := range nativeJSONCodecTypes {
		if t.Implements(codec) || t.Kind() != reflect.Pointer && reflect.PointerTo(t).Implements(codec) {
			return reflect.Value{}, errors.New("native JSON custom codec refused")
		}
	}
	result := reflect.New(t).Elem()
	switch v.Kind() {
	case reflect.String:
		value := v.String()
		if encode {
			value = base64.StdEncoding.EncodeToString([]byte(value))
		} else {
			raw, err := base64.StdEncoding.Strict().DecodeString(value)
			if err != nil {
				return reflect.Value{}, errors.New("native JSON string is not base64")
			}
			value = string(raw)
		}
		result.SetString(value)
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if !field.IsExported() || field.Anonymous {
				return reflect.Value{}, errors.New("native JSON embedded/private field refused")
			}
			value, err := nativeJSONStrings(v.Field(i), encode, depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			result.Field(i).Set(value)
		}
	case reflect.Pointer:
		// Validate the pointed shape even for nil values; omitted descriptors cannot
		// hide a custom-codec or interface field from the decoder's preflight.
		source := reflect.New(t.Elem()).Elem()
		if !v.IsNil() {
			source = v.Elem()
		}
		value, err := nativeJSONStrings(source, encode, depth+1)
		if err != nil {
			return reflect.Value{}, err
		}
		if !v.IsNil() {
			result.Set(reflect.New(t.Elem()))
			result.Elem().Set(value)
		}
	case reflect.Slice, reflect.Array:
		if _, err := nativeJSONStrings(reflect.New(t.Elem()).Elem(), encode, depth+1); err != nil {
			return reflect.Value{}, err
		}
		if v.Kind() == reflect.Slice {
			if v.IsNil() {
				_, err := nativeJSONStrings(reflect.New(t.Elem()).Elem(), encode, depth+1)
				return result, err
			}
			result.Set(reflect.MakeSlice(t, v.Len(), v.Len()))
		}
		if t.Elem().Kind() == reflect.Uint8 {
			reflect.Copy(result, v)
			return result, nil
		}
		for i := 0; i < v.Len(); i++ {
			value, err := nativeJSONStrings(v.Index(i), encode, depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			result.Index(i).Set(value)
		}
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		result.Set(v)
	default:
		return reflect.Value{}, errors.New("native JSON unsupported field kind")
	}
	return result, nil
}
