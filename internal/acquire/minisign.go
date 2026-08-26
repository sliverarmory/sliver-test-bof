package acquire

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/blake2b"
)

const (
	minisignEdDSA     = 0x6445
	minisignHashEdDSA = 0x4445
)

type minisignPublicKey struct {
	id  uint64
	key ed25519.PublicKey
}

type minisignSignature struct {
	algorithm        uint16
	keyID            uint64
	signature        []byte
	trustedComment   string
	commentSignature []byte
}

func parseMinisignPublicKey(text string) (minisignPublicKey, error) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	encoded := strings.TrimSpace(lines[len(lines)-1])
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return minisignPublicKey{}, fmt.Errorf("decode minisign public key: %w", err)
	}
	if len(raw) != 2+8+ed25519.PublicKeySize {
		return minisignPublicKey{}, fmt.Errorf("invalid minisign public key length %d", len(raw))
	}
	if binary.LittleEndian.Uint16(raw[:2]) != minisignEdDSA {
		return minisignPublicKey{}, errors.New("invalid minisign public key algorithm")
	}
	return minisignPublicKey{
		id:  binary.LittleEndian.Uint64(raw[2:10]),
		key: ed25519.PublicKey(append([]byte(nil), raw[10:]...)),
	}, nil
}

func parseMinisignSignature(text []byte) (minisignSignature, error) {
	parts := strings.SplitN(string(text), "\n", 4)
	if len(parts) != 4 {
		return minisignSignature{}, errors.New("invalid minisign signature")
	}
	untrusted := strings.TrimSuffix(parts[0], "\r")
	trusted := strings.TrimSuffix(parts[2], "\r")
	if !strings.HasPrefix(untrusted, "untrusted comment: ") {
		return minisignSignature{}, errors.New("invalid minisign untrusted comment")
	}
	if !strings.HasPrefix(trusted, "trusted comment: ") {
		return minisignSignature{}, errors.New("invalid minisign trusted comment")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(parts[1]))
	if err != nil {
		return minisignSignature{}, fmt.Errorf("decode minisign signature: %w", err)
	}
	if len(raw) != 2+8+ed25519.SignatureSize {
		return minisignSignature{}, fmt.Errorf("invalid minisign signature length %d", len(raw))
	}
	algorithm := binary.LittleEndian.Uint16(raw[:2])
	if algorithm != minisignEdDSA && algorithm != minisignHashEdDSA {
		return minisignSignature{}, errors.New("invalid minisign signature algorithm")
	}
	commentSignature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(parts[3]))
	if err != nil {
		return minisignSignature{}, fmt.Errorf("decode minisign comment signature: %w", err)
	}
	if len(commentSignature) != ed25519.SignatureSize {
		return minisignSignature{}, fmt.Errorf("invalid minisign comment signature length %d", len(commentSignature))
	}
	return minisignSignature{
		algorithm:        algorithm,
		keyID:            binary.LittleEndian.Uint64(raw[2:10]),
		signature:        append([]byte(nil), raw[10:]...),
		trustedComment:   strings.TrimPrefix(trusted, "trusted comment: "),
		commentSignature: commentSignature,
	}, nil
}

func verifyMinisignBytes(publicKeyText string, data, signatureText []byte) (string, error) {
	signature, err := parseMinisignSignature(signatureText)
	if err != nil {
		return "", err
	}
	message := data
	if signature.algorithm == minisignHashEdDSA {
		digest := blake2b.Sum512(data)
		message = digest[:]
	}
	if err := verifyMinisignMessage(publicKeyText, message, signature); err != nil {
		return "", err
	}
	return signature.trustedComment, nil
}

func verifyMinisignReader(publicKeyText string, reader io.Reader, signatureText []byte) (string, error) {
	signature, err := parseMinisignSignature(signatureText)
	if err != nil {
		return "", err
	}
	if signature.algorithm != minisignHashEdDSA {
		return "", errors.New("streamed artifact requires a prehashed minisign signature")
	}
	hash, err := blake2b.New512(nil)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(hash, reader); err != nil {
		return "", fmt.Errorf("hash signed artifact: %w", err)
	}
	if err := verifyMinisignMessage(publicKeyText, hash.Sum(nil), signature); err != nil {
		return "", err
	}
	return signature.trustedComment, nil
}

func verifyMinisignMessage(publicKeyText string, message []byte, signature minisignSignature) error {
	publicKey, err := parseMinisignPublicKey(publicKeyText)
	if err != nil {
		return err
	}
	if signature.keyID != publicKey.id {
		return errors.New("minisign key ID mismatch")
	}
	if !ed25519.Verify(publicKey.key, message, signature.signature) {
		return errors.New("minisign artifact signature verification failed")
	}
	commentMessage := make([]byte, 0, len(signature.signature)+len(signature.trustedComment))
	commentMessage = append(commentMessage, signature.signature...)
	commentMessage = append(commentMessage, signature.trustedComment...)
	if !ed25519.Verify(publicKey.key, commentMessage, signature.commentSignature) {
		return errors.New("minisign trusted comment verification failed")
	}
	return nil
}
