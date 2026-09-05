package v2

import (
	"bytes"
	"encoding/json"
	"io"
)

// Decode strictly decodes and validates one bounded schema-v2 journal.
func Decode(payload []byte) (Record, error) {
	if len(payload) == 0 || len(payload) > MaxBytes {
		return Record{}, invalid("record size is outside the allowed bound")
	}
	var record Record
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return Record{}, invalid("decode: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Record{}, invalid("record contains trailing JSON")
		}
		return Record{}, invalid("decode trailing content: %v", err)
	}
	if err := record.Validate(); err != nil {
		return Record{}, err
	}
	return record, nil
}

// Encode validates a schema-v2 journal and returns its canonical JSON object.
// The durable parent codec owns the historical trailing-newline convention.
func Encode(record Record) ([]byte, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, invalid("encode: %v", err)
	}
	return payload, nil
}

// DecodeArchive strictly decodes and validates one bounded schema-v1 archive
// containing a schema-v2 journal.
func DecodeArchive(payload []byte) (Archive, error) {
	if len(payload) == 0 || len(payload) > MaxBytes {
		return Archive{}, invalid("archive size is outside the allowed bound")
	}
	var archive Archive
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&archive); err != nil {
		return Archive{}, invalid("decode archive: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Archive{}, invalid("archive contains trailing JSON")
		}
		return Archive{}, invalid("decode archive trailing content: %v", err)
	}
	if err := archive.Validate(); err != nil {
		return Archive{}, err
	}
	return archive, nil
}

// EncodeArchive validates a schema-v1 archive and returns its canonical JSON
// object without the durable parent codec's trailing newline.
func EncodeArchive(archive Archive) ([]byte, error) {
	if err := archive.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(archive)
	if err != nil {
		return nil, invalid("encode archive: %v", err)
	}
	return payload, nil
}
