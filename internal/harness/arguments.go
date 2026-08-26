package harness

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"unicode/utf16"
)

const maxArgumentBuffer = 16 << 20

func PackArguments(arguments []Argument, raw []byte) ([]byte, error) {
	if raw != nil {
		if len(arguments) != 0 {
			return nil, fmt.Errorf("typed arguments and raw arguments are mutually exclusive")
		}
		if len(raw) > maxArgumentBuffer {
			return nil, fmt.Errorf("raw BOF argument buffer is %d bytes; maximum is %d", len(raw), maxArgumentBuffer)
		}
		return append([]byte{}, raw...), nil
	}
	payload := new(bytes.Buffer)
	for index, argument := range arguments {
		if err := addArgument(payload, argument); err != nil {
			return nil, fmt.Errorf("argument %d (%s): %w", index+1, argument.Type, err)
		}
		if payload.Len() > maxArgumentBuffer-4 {
			return nil, fmt.Errorf("BOF arguments exceed %d bytes", maxArgumentBuffer)
		}
	}
	result := new(bytes.Buffer)
	if err := binary.Write(result, binary.LittleEndian, uint32(payload.Len())); err != nil {
		return nil, err
	}
	_, _ = result.Write(payload.Bytes())
	return result.Bytes(), nil
}

func addArgument(buffer *bytes.Buffer, argument Argument) error {
	switch argument.Type {
	case "int", "integer":
		if argument.Uint64 > uint64(^uint32(0)) {
			return fmt.Errorf("integer %d exceeds uint32", argument.Uint64)
		}
		return binary.Write(buffer, binary.LittleEndian, uint32(argument.Uint64))
	case "short":
		if argument.Uint64 > uint64(^uint16(0)) {
			return fmt.Errorf("short %d exceeds uint16", argument.Uint64)
		}
		return binary.Write(buffer, binary.LittleEndian, uint16(argument.Uint64))
	case "string":
		return addData(buffer, append([]byte(argument.String), 0))
	case "wstring":
		units := utf16.Encode([]rune(argument.String + "\x00"))
		data := make([]byte, len(units)*2)
		for index, unit := range units {
			binary.LittleEndian.PutUint16(data[index*2:], unit)
		}
		return addData(buffer, data)
	case "file", "bytes", "bytes_base64":
		return addData(buffer, argument.Data)
	default:
		return fmt.Errorf("unsupported argument type %q", argument.Type)
	}
}

func addData(buffer *bytes.Buffer, data []byte) error {
	if uint64(len(data)) > uint64(^uint32(0)) {
		return fmt.Errorf("data is too large")
	}
	if err := binary.Write(buffer, binary.LittleEndian, uint32(len(data))); err != nil {
		return err
	}
	_, err := buffer.Write(data)
	return err
}

func PackCOFFLoaderArguments(entrypoint string, object []byte, inner []byte) ([]byte, error) {
	payload := new(bytes.Buffer)
	if err := addData(payload, append([]byte(entrypoint), 0)); err != nil {
		return nil, err
	}
	if err := addData(payload, object); err != nil {
		return nil, err
	}
	if err := addData(payload, inner); err != nil {
		return nil, err
	}
	result := new(bytes.Buffer)
	if err := binary.Write(result, binary.LittleEndian, uint32(payload.Len())); err != nil {
		return nil, err
	}
	_, _ = result.Write(payload.Bytes())
	return result.Bytes(), nil
}

func DecodeBase64(value string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decode base64: %w", err)
	}
	return data, nil
}
