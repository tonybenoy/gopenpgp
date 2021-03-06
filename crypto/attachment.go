package crypto

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"runtime"
	"sync"

	armorUtils "github.com/ProtonMail/gopenpgp/armor"
	"github.com/ProtonMail/gopenpgp/constants"
	"github.com/ProtonMail/gopenpgp/models"
	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/packet"
)

// AttachmentProcessor keeps track of the progress of encrypting an attachment
// (optimized for encrypting large files).
type AttachmentProcessor struct {
	w                *io.WriteCloser
	pipe             *io.PipeWriter
	done             sync.WaitGroup
	split            *models.EncryptedSplit
	garbageCollector int
	err              error
}

// Process writes attachment data to be encrypted
func (ap *AttachmentProcessor) Process(plainData []byte) {
	if _, err := (*ap.w).Write(plainData); err != nil {
		panic(err)
	}
}

// Finish closes the attachment and returns the encrypted data
func (ap *AttachmentProcessor) Finish() (*models.EncryptedSplit, error) {
	if ap.err != nil {
		return nil, ap.err
	}
	(*ap.w).Close()
	(*ap.pipe).Close()
	ap.done.Wait()
	if ap.garbageCollector > 0 {
		runtime.GC()
	}
	return ap.split, nil
}

// encryptAttachment creates an AttachmentProcessor which can be used to encrypt
// a file. It takes an estimatedSize and fileName as hints about the file.
func (pgp *GopenPGP) encryptAttachment(
	estimatedSize int, fileName string, publicKey *KeyRing, garbageCollector int,
) (*AttachmentProcessor, error) {
	attachmentProc := &AttachmentProcessor{}
	// You could also add these one at a time if needed.
	attachmentProc.done.Add(1)
	attachmentProc.garbageCollector = garbageCollector

	hints := &openpgp.FileHints{
		FileName: fileName,
	}

	config := &packet.Config{
		DefaultCipher: packet.CipherAES256,
		Time:          pgp.getTimeGenerator(),
	}

	reader, writer := io.Pipe()

	go func() {
		defer attachmentProc.done.Done()
		split, splitError := SeparateKeyAndData(nil, reader, estimatedSize, garbageCollector)
		if attachmentProc.err != nil {
			attachmentProc.err = splitError
		}
		split.Algo = constants.AES256
		attachmentProc.split = split
	}()

	var ew io.WriteCloser
	var encryptErr error
	ew, encryptErr = openpgp.Encrypt(writer, publicKey.entities, nil, hints, config)
	if encryptErr != nil {
		return nil, encryptErr
	}
	attachmentProc.w = &ew
	attachmentProc.pipe = writer

	return attachmentProc, nil
}

// EncryptAttachment encrypts a file. fileName
func (pgp *GopenPGP) EncryptAttachment(
	plainData []byte, fileName string, publicKey *KeyRing,
) (*models.EncryptedSplit, error) {
	ap, err := pgp.encryptAttachment(len(plainData), fileName, publicKey, -1)
	if err != nil {
		return nil, err
	}
	ap.Process(plainData)
	split, err := ap.Finish()
	if err != nil {
		return nil, err
	}
	return split, nil
}

// EncryptAttachmentLowMemory creates an AttachmentProcessor which can be used
// to encrypt a file. It takes an estimatedSize and fileName as hints about the
// file. It is optimized for low-memory environments and collects garbage every
// megabyte.
func (pgp *GopenPGP) EncryptAttachmentLowMemory(
	estimatedSize int, fileName string, publicKey *KeyRing,
) (*AttachmentProcessor, error) {
	return pgp.encryptAttachment(estimatedSize, fileName, publicKey, 1<<20)
}

// SplitArmor is a helper method which splits an armored message into its
// session key packet and symmetrically encrypted data packet.
func SplitArmor(encrypted string) (*models.EncryptedSplit, error) {
	var err error

	encryptedRaw, err := armorUtils.Unarmor(encrypted)
	if err != nil {
		return nil, err
	}

	encryptedReader := bytes.NewReader(encryptedRaw)

	return SeparateKeyAndData(nil, encryptedReader, len(encrypted), -1)
}

// DecryptAttachment takes a session key packet and symmetrically encrypted data
// packet. privateKeys is a KeyRing that can contain multiple keys. The
// passphrase is used to unlock keys in privateKeys.
func (pgp *GopenPGP) DecryptAttachment(
	keyPacket, dataPacket []byte,
	kr *KeyRing, passphrase string,
) ([]byte, error) {
	privKeyEntries := kr.entities

	if err := kr.Unlock([]byte(passphrase)); err != nil {
		err = fmt.Errorf("gopenpgp: cannot decrypt attachment: %v", err)
		return nil, err
	}

	keyReader := bytes.NewReader(keyPacket)
	dataReader := bytes.NewReader(dataPacket)

	encryptedReader := io.MultiReader(keyReader, dataReader)

	config := &packet.Config{Time: pgp.getTimeGenerator()}

	md, err := openpgp.ReadMessage(encryptedReader, privKeyEntries, nil, config)
	if err != nil {
		return nil, err
	}

	decrypted := md.UnverifiedBody
	b, err := ioutil.ReadAll(decrypted)
	if err != nil {
		return nil, err
	}

	return b, nil
}
