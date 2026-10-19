package releasetransition

// This is a frozen syntax bridge, not release policy. JournalRecord and
// SupersededJournalRecord retain their existing per-occurrence wire hooks.
import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"reflect"
)

const LegacyCodecMaxRaw = 1 << 20
const LegacyCodecMaxProjection = 8 << 20
const legacyCodecHeader = 52 // magic/version/kind/status/flags/sequence/hash/length

func legacyCodecTarget(kind byte) any {
	switch kind {
	case 1:
		return new(RegistryV2)
	case 2:
		return new([]CapabilityDescriptor)
	case 3:
		return new(LedgerV2)
	case 4:
		return new(LedgerCheckpointV1)
	case 5:
		return new(JournalRecord)
	case 6:
		return new(EvidenceRecord)
	case 7:
		return new(SupersededJournalRecord)
	case 8:
		return new(CompatibilityEvidence)
	case 9:
		return new(RecoveryReceiptV1)
	case 10:
		return new(RecoveryLifecycleRecord)
	case 11:
		return new(RecoveryFrontier)
	case 12:
		return new(MigrationOwnerReceiptV1)
	case 13:
		return new(settingsRecoveryV1)
	case 14:
		return new(ownerRegistrationRecoveryV1)
	case 16:
		// Exact existing checkpoint-capability DTO syntax. Eligibility and the
		// supported version/contract check remain with the Rust owner.
		return new(struct {
			SchemaVersion int    `json:"schemaVersion"`
			Contract      string `json:"contract"`
		})
	case 17:
		return new(struct {
			SchemaVersion int `json:"schemaVersion"`
			Files         []struct {
				Path string `json:"path"`
			} `json:"files"`
		})
	case 18:
		return new(legacyV1Transaction)
	case 19:
		return new(sourceCapabilitySyntax)
	case 20:
		return new(legacyV1Registry)
	case 21:
		return new(legacyV1RouteConsumers)
	default:
		return nil
	}
}

// Upper bound before json.Marshal allocates its output buffer. This walks only
// these closed DTOs, charges omitted fields too, and never invokes wire hooks.
// Strings include worst-case HTML/Unicode escaping; []byte uses padded base64.
func legacyProjectionCost(v reflect.Value, left *int64) bool {
	charge := func(n int64) bool {
		if n < 0 || n > *left {
			return false
		}
		*left -= n
		return true
	}
	if !v.IsValid() {
		return charge(4)
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return charge(4)
		}
		return legacyProjectionCost(v.Elem(), left)
	case reflect.String:
		return charge(2 + 6*int64(v.Len()))
	case reflect.Bool:
		return charge(5)
	case reflect.Int, reflect.Int64, reflect.Int32, reflect.Uint, reflect.Uint64, reflect.Uint32:
		return charge(21)
	case reflect.Slice:
		if v.IsNil() {
			return charge(4)
		}
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return charge(2 + 4*((int64(v.Len())+2)/3))
		}
		fallthrough
	case reflect.Array:
		if !charge(2) {
			return false
		}
		for i := 0; i < v.Len(); i++ {
			if !charge(1) || !legacyProjectionCost(v.Index(i), left) {
				return false
			}
		}
		return true
	case reflect.Map:
		if v.IsNil() {
			return charge(4)
		}
		if !charge(2) {
			return false
		}
		it := v.MapRange()
		for it.Next() {
			if !charge(2) || !legacyProjectionCost(it.Key(), left) || !legacyProjectionCost(it.Value(), left) {
				return false
			}
		}
		return true
	case reflect.Struct:
		if !charge(2) {
			return false
		}
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if f.PkgPath != "" || f.Tag.Get("json") == "-" {
				continue
			}
			if !charge(int64(len(f.Name)+len(f.Tag.Get("json")))+5) || !legacyProjectionCost(v.Field(i), left) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
func normalizeLegacy(kind byte, raw []byte) ([]byte, byte, error) {
	target := legacyCodecTarget(kind)
	if target == nil || len(raw) == 0 || len(raw) > LegacyCodecMaxRaw {
		return nil, 0, errors.New("bound or kind")
	}
	// Same strict syntax decoder, without any Parse*/Validate policy calls.
	if kind == 17 {
		// Config-layout rollback used json.Unmarshal, which permits unknown
		// fields. Preserve that grammar without importing its eligibility rules.
		if err := json.Unmarshal(raw, target); err != nil {
			return nil, 0, err
		}
	} else {
		if err := decodeBoundedRecord(raw, LegacyCodecMaxRaw, target); err != nil {
			return nil, 0, err
		}
	}
	flags := byte(0)
	if kind == 9 {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, 0, err
		}
		for i, key := range []string{"basePlan", "reservation", "generation"} {
			if _, ok := fields[key]; ok {
				flags |= 1 << i
			}
		}
	}
	budget := int64(LegacyCodecMaxProjection)
	if !legacyProjectionCost(reflect.ValueOf(target), &budget) {
		return nil, 0, errors.New("projection bound")
	}
	normalized, err := json.Marshal(target)
	if err != nil || len(normalized) > LegacyCodecMaxProjection {
		return nil, 0, errors.New("projection encoding")
	}
	return normalized, flags, nil
}

// NormalizeFrozenRecord is the syntax-only arm reused by the retained native
// helper. It never selects release work or reads a protected store.
func NormalizeFrozenRecord(kind byte, raw []byte) ([]byte, byte, error) {
	return normalizeLegacy(kind, raw)
}

// RunLegacyCodec is a retained, read-only fixed-kind framed stdin/stdout peer.
// EOF between frames ends the session; a malformed frame poisons it permanently.
func RunLegacyCodec(input io.Reader, output io.Writer) error {
	for count := uint64(1); count != 0; count++ {
		header := make([]byte, legacyCodecHeader)
		n, err := io.ReadFull(input, header)
		if err == io.EOF && n == 0 {
			return nil
		}
		if err != nil {
			return errors.New("truncated codec frame")
		}
		size := binary.BigEndian.Uint32(header[48:])
		if !bytes.Equal(header[:4], []byte("SYC1")) || header[4] != 1 || header[6] != 0 || header[7] != 0 || binary.BigEndian.Uint64(header[8:16]) != count || size == 0 || size > LegacyCodecMaxRaw || legacyCodecTarget(header[5]) == nil {
			return errors.New("invalid codec frame")
		}
		raw := make([]byte, int(size))
		if _, err := io.ReadFull(input, raw); err != nil {
			return errors.New("truncated codec payload")
		}
		hash := sha256.Sum256(raw)
		if !bytes.Equal(hash[:], header[16:48]) {
			return errors.New("codec input binding")
		}
		normalized, flags, err := normalizeLegacy(header[5], raw)
		if err != nil {
			header[6] = 1
			normalized = nil
		}
		header[7] = flags
		binary.BigEndian.PutUint32(header[48:], uint32(len(normalized)))
		if _, err := output.Write(header); err != nil {
			return err
		}
		if _, err := output.Write(normalized); err != nil {
			return err
		}
	}
	return errors.New("codec session request bound")
}
