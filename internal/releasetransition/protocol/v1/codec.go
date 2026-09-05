package v1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const maximumJSONBytes = 1 << 20

func DecodeRequest(reader io.Reader) (Request, error) {
	var request Request
	if err := decode(reader, &request); err != nil {
		return Request{}, fmt.Errorf("decode v1 request: %w", err)
	}
	if err := request.validateWire(); err != nil {
		return Request{}, err
	}
	return request, nil
}

func DecodeResponse(reader io.Reader) (Response, error) {
	var response Response
	if err := decode(reader, &response); err != nil {
		return Response{}, fmt.Errorf("decode v1 response: %w", err)
	}
	if err := response.validateWire(); err != nil {
		return Response{}, err
	}
	return response, nil
}

func EncodeRequest(writer io.Writer, request Request) error {
	if err := request.validateWire(); err != nil {
		return err
	}
	return encode(writer, request)
}

func EncodeResponse(writer io.Writer, response Response) error {
	if err := response.validateWire(); err != nil {
		return err
	}
	return encode(writer, response)
}

func decode(reader io.Reader, destination any) error {
	if reader == nil {
		return errors.New("v1 protocol reader is required")
	}
	payload, err := io.ReadAll(io.LimitReader(reader, maximumJSONBytes+1))
	if err != nil {
		return err
	}
	if len(payload) > maximumJSONBytes {
		return errors.New("v1 protocol JSON exceeds 1 MiB")
	}
	var header struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(payload, &header); err != nil {
		return err
	}
	if header.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported release transition schema %d", header.SchemaVersion)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("v1 protocol JSON contains trailing data")
		}
		return err
	}
	return nil
}

func encode(writer io.Writer, value any) error {
	if writer == nil {
		return errors.New("v1 protocol writer is required")
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(payload)+1 > maximumJSONBytes {
		return errors.New("v1 protocol JSON exceeds 1 MiB")
	}
	payload = append(payload, '\n')
	written, err := writer.Write(payload)
	if err != nil {
		return err
	}
	if written != len(payload) {
		return io.ErrShortWrite
	}
	return nil
}
