package backup

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

var archiveMagic = [8]byte{'A', 'I', 'C', 'O', 'B', 'K', 'P', '1'}

const (
	headerPrefixSize    = 8 + 2 + 4
	recordHeaderSize    = 1 + 8 + 4
	recordData          = byte(1)
	recordFooter        = byte(2)
	maxPublicHeaderSize = 64 * 1024
)

type KeyMaterial struct {
	Header PublicHeader
	key    [chacha20poly1305.KeySize]byte
}

// NewPlainMaterial creates a password-free archive. SHA-256 frames detect
// corruption only; they provide no confidentiality or tamper authentication.
func NewPlainMaterial(createdAt time.Time) *KeyMaterial {
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	return &KeyMaterial{Header: PublicHeader{
		Format: FormatName, Version: FormatVersion, CreatedAt: createdAt.UTC(),
		Protection: Protection{Algorithm: "sha256-chunked", KDF: "none", ChunkSize: DefaultChunkSize},
	}}
}

type checksumFrames struct{}

func (checksumFrames) NonceSize() int { return chacha20poly1305.NonceSizeX }
func (checksumFrames) Overhead() int  { return sha256.Size }
func (checksumFrames) Seal(dst, _nonce, plaintext, additionalData []byte) []byte {
	digest := frameChecksum(additionalData, plaintext)
	dst = append(dst, plaintext...)
	return append(dst, digest[:]...)
}
func (checksumFrames) Open(dst, _nonce, payload, additionalData []byte) ([]byte, error) {
	if len(payload) < sha256.Size {
		return nil, errors.New("frame checksum is missing")
	}
	plaintext := payload[:len(payload)-sha256.Size]
	digest := frameChecksum(additionalData, plaintext)
	if !bytes.Equal(digest[:], payload[len(plaintext):]) {
		return nil, errors.New("frame checksum mismatch")
	}
	return append(dst, plaintext...), nil
}
func frameChecksum(additionalData, plaintext []byte) [sha256.Size]byte {
	h := sha256.New()
	_, _ = h.Write([]byte("aico-backup-sha256-frame-v1"))
	_, _ = h.Write(additionalData)
	_, _ = h.Write(plaintext)
	var result [sha256.Size]byte
	copy(result[:], h.Sum(nil))
	return result
}
func frameProtection(material *KeyMaterial) (cipherAEAD, error) {
	if material.Header.Protection.Algorithm == "sha256-chunked" {
		return checksumFrames{}, nil
	}
	return chacha20poly1305.NewX(material.key[:])
}

func NewKeyMaterial(passphrase []byte, createdAt time.Time) (*KeyMaterial, error) {
	salt := make([]byte, 16)
	noncePrefix := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generate backup salt: %w", err)
	}
	if _, err := rand.Read(noncePrefix); err != nil {
		return nil, fmt.Errorf("generate backup nonce: %w", err)
	}
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	header := PublicHeader{
		Format: FormatName, Version: FormatVersion, CreatedAt: createdAt.UTC(),
		Protection: Protection{
			Algorithm: "xchacha20-poly1305-chunked", KDF: "argon2id",
			Salt:        base64.StdEncoding.EncodeToString(salt),
			NoncePrefix: base64.StdEncoding.EncodeToString(noncePrefix),
			MemoryKiB:   DefaultMemoryKiB, Iterations: DefaultIterations,
			Parallelism: DefaultParallelism, ChunkSize: DefaultChunkSize,
		},
	}
	key, err := deriveKey(header, passphrase)
	if err != nil {
		return nil, err
	}
	return &KeyMaterial{Header: header, key: key}, nil
}

func DeriveKeyMaterial(header PublicHeader, passphrase []byte) (*KeyMaterial, error) {
	key, err := deriveKey(header, passphrase)
	if err != nil {
		return nil, err
	}
	return &KeyMaterial{Header: header, key: key}, nil
}

func deriveKey(header PublicHeader, passphrase []byte) ([chacha20poly1305.KeySize]byte, error) {
	var result [chacha20poly1305.KeySize]byte
	if err := header.Validate(); err != nil {
		return result, errorf("unsupported_format", err, "invalid backup header: %v", err)
	}
	if header.Protection.Algorithm == "sha256-chunked" {
		return result, nil
	}
	if len(passphrase) < 8 {
		return result, errorf("wrong_passphrase", nil, "backup passphrase must contain at least 8 bytes")
	}
	salt, err := base64.StdEncoding.DecodeString(header.Protection.Salt)
	if err != nil || len(salt) != 16 {
		return result, errorf("manifest_invalid", err, "backup header contains an invalid salt")
	}
	noncePrefix, err := base64.StdEncoding.DecodeString(header.Protection.NoncePrefix)
	if err != nil || len(noncePrefix) != 16 {
		return result, errorf("manifest_invalid", err, "backup header contains an invalid nonce prefix")
	}
	derived := argon2.IDKey(passphrase, salt, header.Protection.Iterations,
		header.Protection.MemoryKiB, header.Protection.Parallelism, chacha20poly1305.KeySize)
	copy(result[:], derived)
	zeroBytes(derived)
	return result, nil
}

func (m *KeyMaterial) Destroy() {
	if m == nil {
		return
	}
	zeroBytes(m.key[:])
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func MarshalPublicHeader(header PublicHeader) ([]byte, error) {
	if err := header.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}
	if len(data) > maxPublicHeaderSize {
		return nil, fmt.Errorf("public header is too large")
	}
	return data, nil
}

func WritePublicHeader(writer io.Writer, header PublicHeader) ([]byte, error) {
	data, err := MarshalPublicHeader(header)
	if err != nil {
		return nil, err
	}
	prefix := make([]byte, headerPrefixSize)
	copy(prefix[:8], archiveMagic[:])
	binary.BigEndian.PutUint16(prefix[8:10], uint16(FormatVersion))
	binary.BigEndian.PutUint32(prefix[10:14], uint32(len(data)))
	if _, err := writer.Write(prefix); err != nil {
		return nil, err
	}
	if _, err := writer.Write(data); err != nil {
		return nil, err
	}
	return data, nil
}

func publicHeaderPrefix(data []byte) []byte {
	prefix := make([]byte, headerPrefixSize+len(data))
	copy(prefix[:8], archiveMagic[:])
	binary.BigEndian.PutUint16(prefix[8:10], uint16(FormatVersion))
	binary.BigEndian.PutUint32(prefix[10:14], uint32(len(data)))
	copy(prefix[headerPrefixSize:], data)
	return prefix
}

func ReadPublicHeader(reader io.Reader) (PublicHeader, []byte, error) {
	var header PublicHeader
	prefix := make([]byte, headerPrefixSize)
	if _, err := io.ReadFull(reader, prefix); err != nil {
		return header, nil, errorf("archive_truncated", err, "backup header is truncated")
	}
	if !bytes.Equal(prefix[:8], archiveMagic[:]) {
		return header, nil, errorf("unsupported_format", nil, "file is not an Aico Link backup")
	}
	if version := binary.BigEndian.Uint16(prefix[8:10]); version != FormatVersion {
		return header, nil, errorf("unsupported_format", nil, "unsupported backup container version %d", version)
	}
	length := binary.BigEndian.Uint32(prefix[10:14])
	if length == 0 || length > maxPublicHeaderSize {
		return header, nil, errorf("manifest_invalid", nil, "invalid public header length %d", length)
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(reader, data); err != nil {
		return header, nil, errorf("archive_truncated", err, "backup public header is truncated")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&header); err != nil {
		return header, nil, errorf("manifest_invalid", err, "invalid backup public header")
	}
	if err := header.Validate(); err != nil {
		return header, nil, errorf("unsupported_format", err, "invalid backup public header: %v", err)
	}
	return header, data, nil
}

type encryptedWriter struct {
	writer        io.Writer
	aead          cipherAEAD
	headerHash    [sha256.Size]byte
	noncePrefix   [16]byte
	chunkSize     int
	buffer        []byte
	sequence      uint64
	dataFrames    uint64
	plaintext     int64
	plaintextHash hash.Hash
	closed        bool
}

type cipherAEAD interface {
	NonceSize() int
	Overhead() int
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
}

func newEncryptedWriter(writer io.Writer, material *KeyMaterial) (*encryptedWriter, error) {
	if material == nil {
		return nil, fmt.Errorf("backup key material is missing")
	}
	headerData, err := WritePublicHeader(writer, material.Header)
	if err != nil {
		return nil, err
	}
	aead, err := frameProtection(material)
	if err != nil {
		return nil, err
	}
	prefix, _ := base64.StdEncoding.DecodeString(material.Header.Protection.NoncePrefix)
	result := &encryptedWriter{
		writer: writer, aead: aead, headerHash: sha256.Sum256(headerData),
		chunkSize:     material.Header.Protection.ChunkSize,
		buffer:        make([]byte, 0, material.Header.Protection.ChunkSize),
		plaintextHash: sha256.New(),
	}
	copy(result.noncePrefix[:], prefix)
	return result, nil
}

func (w *encryptedWriter) Write(data []byte) (int, error) {
	if w.closed {
		return 0, fmt.Errorf("write to closed encrypted backup stream")
	}
	written := 0
	for len(data) > 0 {
		available := w.chunkSize - len(w.buffer)
		count := len(data)
		if count > available {
			count = available
		}
		w.buffer = append(w.buffer, data[:count]...)
		data = data[count:]
		written += count
		if len(w.buffer) == w.chunkSize {
			if err := w.flushData(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

func (w *encryptedWriter) flushData() error {
	if len(w.buffer) == 0 {
		return nil
	}
	_, _ = w.plaintextHash.Write(w.buffer)
	w.plaintext += int64(len(w.buffer))
	if err := w.writeRecord(recordData, w.buffer); err != nil {
		return err
	}
	w.dataFrames++
	w.buffer = w.buffer[:0]
	return nil
}

type authenticatedFooter struct {
	DataFrames      uint64 `json:"data_frames"`
	PlaintextBytes  int64  `json:"plaintext_bytes"`
	PlaintextSHA256 string `json:"plaintext_sha256"`
}

func (w *encryptedWriter) Close() error {
	if w.closed {
		return nil
	}
	if err := w.flushData(); err != nil {
		return err
	}
	footer, err := json.Marshal(authenticatedFooter{
		DataFrames: w.dataFrames, PlaintextBytes: w.plaintext,
		PlaintextSHA256: hex.EncodeToString(w.plaintextHash.Sum(nil)),
	})
	if err != nil {
		return err
	}
	if err := w.writeRecord(recordFooter, footer); err != nil {
		return err
	}
	w.closed = true
	zeroBytes(w.buffer)
	return nil
}

func (w *encryptedWriter) writeRecord(recordType byte, plaintext []byte) error {
	nonce := makeNonce(w.noncePrefix, w.sequence)
	aad := makeAAD(w.headerHash, recordType, w.sequence)
	ciphertext := w.aead.Seal(nil, nonce, plaintext, aad)
	header := make([]byte, recordHeaderSize)
	header[0] = recordType
	binary.BigEndian.PutUint64(header[1:9], w.sequence)
	binary.BigEndian.PutUint32(header[9:13], uint32(len(ciphertext)))
	if _, err := w.writer.Write(header); err != nil {
		return err
	}
	if _, err := w.writer.Write(ciphertext); err != nil {
		return err
	}
	w.sequence++
	return nil
}

type decryptedReader struct {
	reader        io.Reader
	aead          cipherAEAD
	headerHash    [sha256.Size]byte
	noncePrefix   [16]byte
	chunkSize     int
	sequence      uint64
	dataFrames    uint64
	plaintext     int64
	plaintextHash hash.Hash
	buffer        []byte
	footerSeen    bool
	terminalErr   error
}

func newDecryptedReader(reader io.Reader, material *KeyMaterial) (*decryptedReader, error) {
	header, headerData, err := ReadPublicHeader(reader)
	if err != nil {
		return nil, err
	}
	if material == nil || header != material.Header {
		return nil, errorf("manifest_invalid", nil, "backup public header changed after key derivation")
	}
	aead, err := frameProtection(material)
	if err != nil {
		return nil, err
	}
	prefix, _ := base64.StdEncoding.DecodeString(header.Protection.NoncePrefix)
	result := &decryptedReader{
		reader: reader, aead: aead, headerHash: sha256.Sum256(headerData),
		chunkSize: header.Protection.ChunkSize, plaintextHash: sha256.New(),
	}
	copy(result.noncePrefix[:], prefix)
	return result, nil
}

func (r *decryptedReader) Read(target []byte) (int, error) {
	for len(r.buffer) == 0 && r.terminalErr == nil {
		r.readRecord()
	}
	if len(r.buffer) > 0 {
		count := copy(target, r.buffer)
		r.buffer = r.buffer[count:]
		return count, nil
	}
	return 0, r.terminalErr
}

func (r *decryptedReader) readRecord() {
	header := make([]byte, recordHeaderSize)
	if _, err := io.ReadFull(r.reader, header); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			r.terminalErr = errorf("archive_truncated", err, "backup ended before its authenticated footer")
		} else {
			r.terminalErr = err
		}
		return
	}
	recordType := header[0]
	sequence := binary.BigEndian.Uint64(header[1:9])
	length := binary.BigEndian.Uint32(header[9:13])
	if sequence != r.sequence {
		r.terminalErr = errorf("archive_authentication_failed", nil, "encrypted frame sequence is out of order")
		return
	}
	maxLength := uint32(r.chunkSize + r.aead.Overhead())
	if recordType == recordFooter {
		maxLength = 16*1024 + uint32(r.aead.Overhead())
	}
	if length < uint32(r.aead.Overhead()) || length > maxLength {
		r.terminalErr = errorf("archive_authentication_failed", nil, "encrypted frame length is invalid")
		return
	}
	ciphertext := make([]byte, length)
	if _, err := io.ReadFull(r.reader, ciphertext); err != nil {
		r.terminalErr = errorf("archive_truncated", err, "encrypted frame is truncated")
		return
	}
	nonce := makeNonce(r.noncePrefix, sequence)
	aad := makeAAD(r.headerHash, recordType, sequence)
	plaintext, err := r.aead.Open(nil, nonce, ciphertext, aad)
	zeroBytes(ciphertext)
	if err != nil {
		if _, plain := r.aead.(checksumFrames); plain {
			r.terminalErr = errorf("hash_mismatch", err, "backup frame checksum failed")
		} else {
			r.terminalErr = errorf("wrong_passphrase", err, "backup authentication failed")
		}
		return
	}
	r.sequence++
	switch recordType {
	case recordData:
		if r.footerSeen {
			r.terminalErr = errorf("archive_authentication_failed", nil, "data follows the authenticated footer")
			zeroBytes(plaintext)
			return
		}
		_, _ = r.plaintextHash.Write(plaintext)
		r.plaintext += int64(len(plaintext))
		r.dataFrames++
		r.buffer = plaintext
	case recordFooter:
		if r.footerSeen {
			r.terminalErr = errorf("archive_authentication_failed", nil, "backup contains multiple footers")
			zeroBytes(plaintext)
			return
		}
		r.footerSeen = true
		var footer authenticatedFooter
		if err := json.Unmarshal(plaintext, &footer); err != nil {
			r.terminalErr = errorf("archive_authentication_failed", err, "authenticated footer is invalid")
			zeroBytes(plaintext)
			return
		}
		zeroBytes(plaintext)
		if footer.DataFrames != r.dataFrames || footer.PlaintextBytes != r.plaintext ||
			footer.PlaintextSHA256 != hex.EncodeToString(r.plaintextHash.Sum(nil)) {
			r.terminalErr = errorf("archive_authentication_failed", nil, "authenticated footer does not match the payload")
			return
		}
		var trailing [1]byte
		count, err := io.ReadFull(r.reader, trailing[:])
		if count != 0 || (err != nil && !errors.Is(err, io.EOF)) {
			r.terminalErr = errorf("archive_authentication_failed", err, "backup contains data after its authenticated footer")
			return
		}
		r.terminalErr = io.EOF
	default:
		zeroBytes(plaintext)
		r.terminalErr = errorf("archive_authentication_failed", nil, "unknown encrypted frame type %d", recordType)
	}
}

func makeNonce(prefix [16]byte, sequence uint64) []byte {
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	copy(nonce, prefix[:])
	binary.BigEndian.PutUint64(nonce[16:], sequence)
	return nonce
}

func makeAAD(headerHash [sha256.Size]byte, recordType byte, sequence uint64) []byte {
	aad := make([]byte, sha256.Size+2+1+8)
	copy(aad, headerHash[:])
	binary.BigEndian.PutUint16(aad[sha256.Size:sha256.Size+2], uint16(FormatVersion))
	aad[sha256.Size+2] = recordType
	binary.BigEndian.PutUint64(aad[sha256.Size+3:], sequence)
	return aad
}
