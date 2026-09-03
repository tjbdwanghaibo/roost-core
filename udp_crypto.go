package nettransport

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"sync"
	"sync/atomic"

	core "github.com/tjbdwanghaibo/roost-core/statesync"
)

const udpEnvelopeHeaderSize = 16 // session ID + packet sequence

// AEADSessionProtector authenticates the UDP routing header, encrypts the
// replication packet, and rejects duplicate or stale packets with a 64-packet
// replay window. SendSalt and ReceiveSalt must be exchanged by an authenticated
// handshake and must differ for the two directions.
type AEADSessionProtector struct {
	aead        cipher.AEAD
	sendSalt    [4]byte
	receiveSalt [4]byte
	sendSeq     atomic.Uint64

	receiveMu   sync.Mutex
	receiveMax  uint64
	receiveMask uint64
}

func NewAESGCMProtector(key []byte, sendSalt, receiveSalt [4]byte) (*AEADSessionProtector, error) {
	if sendSalt == receiveSalt {
		return nil, ErrProtocolConfig
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &AEADSessionProtector{aead: aead, sendSalt: sendSalt, receiveSalt: receiveSalt}, nil
}

func RandomNonceSalt() ([4]byte, error) {
	var salt [4]byte
	_, err := io.ReadFull(rand.Reader, salt[:])
	return salt, err
}

func (protector *AEADSessionProtector) Overhead() int {
	if protector == nil || protector.aead == nil {
		return 0
	}
	return udpEnvelopeHeaderSize + protector.aead.Overhead()
}

func (protector *AEADSessionProtector) Seal(session core.SessionID, payload []byte) ([]byte, error) {
	if protector == nil || protector.aead == nil || session == 0 || len(payload) == 0 {
		return nil, ErrProtocolConfig
	}
	sequence, ok := protector.nextSequence()
	if !ok {
		return nil, ErrTransportClosed
	}
	header := make([]byte, udpEnvelopeHeaderSize, udpEnvelopeHeaderSize+len(payload)+protector.aead.Overhead())
	binary.BigEndian.PutUint64(header[0:8], uint64(session))
	binary.BigEndian.PutUint64(header[8:16], sequence)
	nonce := protector.nonce(protector.sendSalt, sequence)
	return protector.aead.Seal(header, nonce[:], payload, header), nil
}

func (protector *AEADSessionProtector) nextSequence() (uint64, bool) {
	for {
		current := protector.sendSeq.Load()
		if current == math.MaxUint64 {
			return 0, false
		}
		if protector.sendSeq.CompareAndSwap(current, current+1) {
			return current + 1, true
		}
	}
}

func (protector *AEADSessionProtector) Open(expected core.SessionID, packet []byte) ([]byte, error) {
	if protector == nil || protector.aead == nil || expected == 0 || len(packet) < protector.Overhead() {
		return nil, ErrAuthentication
	}
	session := core.SessionID(binary.BigEndian.Uint64(packet[0:8]))
	sequence := binary.BigEndian.Uint64(packet[8:16])
	if session != expected || sequence == 0 {
		return nil, ErrAuthentication
	}
	protector.receiveMu.Lock()
	defer protector.receiveMu.Unlock()
	if protector.replayed(sequence) {
		return nil, ErrAuthentication
	}
	nonce := protector.nonce(protector.receiveSalt, sequence)
	plain, err := protector.aead.Open(nil, nonce[:], packet[udpEnvelopeHeaderSize:], packet[:udpEnvelopeHeaderSize])
	if err != nil {
		return nil, errors.Join(ErrAuthentication, err)
	}
	protector.markReceived(sequence)
	return plain, nil
}

func (*AEADSessionProtector) nonce(salt [4]byte, sequence uint64) [12]byte {
	var nonce [12]byte
	copy(nonce[0:4], salt[:])
	binary.BigEndian.PutUint64(nonce[4:12], sequence)
	return nonce
}

func (protector *AEADSessionProtector) replayed(sequence uint64) bool {
	if protector.receiveMax == 0 || sequence > protector.receiveMax {
		return false
	}
	distance := protector.receiveMax - sequence
	return distance >= 64 || protector.receiveMask&(uint64(1)<<distance) != 0
}

func (protector *AEADSessionProtector) markReceived(sequence uint64) {
	if protector.receiveMax == 0 {
		protector.receiveMax = sequence
		protector.receiveMask = 1
		return
	}
	if sequence > protector.receiveMax {
		shift := sequence - protector.receiveMax
		if shift >= 64 {
			protector.receiveMask = 1
		} else {
			protector.receiveMask = protector.receiveMask<<shift | 1
		}
		protector.receiveMax = sequence
		return
	}
	protector.receiveMask |= uint64(1) << (protector.receiveMax - sequence)
}
