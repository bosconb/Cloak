package multiplex

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/salsa20"
)

type Obfser func(*Frame, []byte) (int, error)
type Deobfser func([]byte) (*Frame, error)

var u32 = binary.BigEndian.Uint32
var u64 = binary.BigEndian.Uint64
var putU32 = binary.BigEndian.PutUint32
var putU64 = binary.BigEndian.PutUint64

const HEADER_LEN = 14

const (
	E_METHOD_PLAIN = iota
	E_METHOD_AES_GCM
	E_METHOD_CHACHA20_POLY1305
)

func MakeObfs(salsaKey [32]byte, payloadCipher cipher.AEAD, hasRecordLayer bool) Obfser {
	var rlLen int
	if hasRecordLayer {
		rlLen = 5
	}
	obfs := func(f *Frame, buf []byte) (int, error) {
		// we need the encrypted data to be at least 8 bytes to be used as nonce for salsa20 stream header encryption
		// this will be the case if the encryption method is an AEAD cipher, however for plain, it's well possible
		// that the frame payload is smaller than 8 bytes, so we need to add on the difference
		var extraLen uint8
		if payloadCipher == nil {
			if len(f.Payload) < 8 {
				extraLen = uint8(8 - len(f.Payload))
			}
		} else {
			extraLen = uint8(payloadCipher.Overhead())
		}

		// usefulLen is the amount of bytes that will be eventually sent off
		usefulLen := rlLen + HEADER_LEN + len(f.Payload) + int(extraLen)
		if len(buf) < usefulLen {
			return 0, errors.New("buffer is too small")

		}
		// we do as much in-place as possible to save allocation
		useful := buf[:usefulLen] // (tls header) + payload + potential overhead
		header := useful[rlLen : rlLen+HEADER_LEN]
		encryptedPayloadWithExtra := useful[rlLen+HEADER_LEN:]

		putU32(header[0:4], f.StreamID)
		putU64(header[4:12], f.Seq)
		header[12] = f.Closing
		header[13] = extraLen

		if payloadCipher == nil {
			copy(encryptedPayloadWithExtra, f.Payload)
			if extraLen != 0 {
				rand.Read(encryptedPayloadWithExtra[len(encryptedPayloadWithExtra)-int(extraLen):])
			}
		} else {
			ciphertext := payloadCipher.Seal(nil, header[:12], f.Payload, nil)
			copy(encryptedPayloadWithExtra, ciphertext)
		}

		nonce := encryptedPayloadWithExtra[len(encryptedPayloadWithExtra)-8:]
		salsa20.XORKeyStream(header, header, nonce, &salsaKey)

		if hasRecordLayer {
			recordLayer := useful[0:5]
			// We don't use util.AddRecordLayer here to avoid unnecessary malloc
			recordLayer[0] = 0x17
			recordLayer[1] = 0x03
			recordLayer[2] = 0x03
			binary.BigEndian.PutUint16(recordLayer[3:5], uint16(HEADER_LEN+len(encryptedPayloadWithExtra)))
		}
		// Composing final obfsed message
		return usefulLen, nil
	}
	return obfs
}

func MakeDeobfs(salsaKey [32]byte, payloadCipher cipher.AEAD, hasRecordLayer bool) Deobfser {
	var rlLen int
	if hasRecordLayer {
		rlLen = 5
	}
	deobfs := func(in []byte) (*Frame, error) {
		if len(in) < rlLen+HEADER_LEN+8 {
			return nil, fmt.Errorf("Input cannot be shorter than %v bytes", rlLen+HEADER_LEN+8)
		}

		peeled := make([]byte, len(in)-rlLen)
		copy(peeled, in[rlLen:])

		header := peeled[:HEADER_LEN]
		pldWithOverHead := peeled[HEADER_LEN:] // payload + potential overhead

		nonce := peeled[len(peeled)-8:]
		salsa20.XORKeyStream(header, header, nonce, &salsaKey)

		streamID := u32(header[0:4])
		seq := u64(header[4:12])
		closing := header[12]
		extraLen := header[13]

		usefulPayloadLen := len(pldWithOverHead) - int(extraLen)
		if usefulPayloadLen < 0 {
			return nil, errors.New("extra length is greater than total pldWithOverHead length")
		}

		var outputPayload []byte

		if payloadCipher == nil {
			if extraLen == 0 {
				outputPayload = pldWithOverHead
			} else {
				outputPayload = pldWithOverHead[:usefulPayloadLen]
			}
		} else {
			_, err := payloadCipher.Open(pldWithOverHead[:0], header[:12], pldWithOverHead, nil)
			if err != nil {
				return nil, err
			}
			outputPayload = pldWithOverHead[:usefulPayloadLen]
		}

		ret := &Frame{
			StreamID: streamID,
			Seq:      seq,
			Closing:  closing,
			Payload:  outputPayload,
		}
		return ret, nil
	}
	return deobfs
}

func GenerateObfs(encryptionMethod byte, sessionKey []byte, hasRecordLayer bool) (obfuscator *Obfuscator, err error) {
	if len(sessionKey) != 32 {
		err = errors.New("sessionKey size must be 32 bytes")
	}

	var salsaKey [32]byte
	copy(salsaKey[:], sessionKey)

	var payloadCipher cipher.AEAD
	switch encryptionMethod {
	case E_METHOD_PLAIN:
		payloadCipher = nil
	case E_METHOD_AES_GCM:
		var c cipher.Block
		c, err = aes.NewCipher(sessionKey)
		if err != nil {
			return
		}
		payloadCipher, err = cipher.NewGCM(c)
		if err != nil {
			return
		}
	case E_METHOD_CHACHA20_POLY1305:
		payloadCipher, err = chacha20poly1305.New(sessionKey)
		if err != nil {
			return
		}
	default:
		return nil, errors.New("Unknown encryption method")
	}

	obfuscator = &Obfuscator{
		MakeObfs(salsaKey, payloadCipher, hasRecordLayer),
		MakeDeobfs(salsaKey, payloadCipher, hasRecordLayer),
		sessionKey,
	}
	return
}
