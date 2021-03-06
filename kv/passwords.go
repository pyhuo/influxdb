package kv

import (
	"context"
	"fmt"

	"golang.org/x/crypto/bcrypt"

	"github.com/influxdata/influxdb/v2"
)

// MinPasswordLength is the shortest password we allow into the system.
const MinPasswordLength = 8

var (
	// EIncorrectPassword is returned when any password operation fails in which
	// we do not want to leak information.
	EIncorrectPassword = &influxdb.Error{
		Code: influxdb.EForbidden,
		Msg:  "your username or password is incorrect",
	}

	// EIncorrectUser is returned when any user is failed to be found which indicates
	// the userID provided is for a user that does not exist.
	EIncorrectUser = &influxdb.Error{
		Code: influxdb.EForbidden,
		Msg:  "your userID is incorrect",
	}

	// EShortPassword is used when a password is less than the minimum
	// acceptable password length.
	EShortPassword = &influxdb.Error{
		Code: influxdb.EInvalid,
		Msg:  "passwords must be at least 8 characters long",
	}
)

// UnavailablePasswordServiceError is used if we aren't able to add the
// password to the store, it means the store is not available at the moment
// (e.g. network).
func UnavailablePasswordServiceError(err error) *influxdb.Error {
	return &influxdb.Error{
		Code: influxdb.EUnavailable,
		Msg:  fmt.Sprintf("Unable to connect to password service. Please try again; Err: %v", err),
		Op:   "kv/setPassword",
	}
}

// CorruptUserIDError is used when the ID was encoded incorrectly previously.
// This is some sort of internal server error.
func CorruptUserIDError(userID string, err error) *influxdb.Error {
	return &influxdb.Error{
		Code: influxdb.EInternal,
		Msg:  fmt.Sprintf("User ID %s has been corrupted; Err: %v", userID, err),
		Op:   "kv/setPassword",
	}
}

// InternalPasswordHashError is used if the hasher is unable to generate
// a hash of the password.  This is some sort of internal server error.
func InternalPasswordHashError(err error) *influxdb.Error {
	return &influxdb.Error{
		Code: influxdb.EInternal,
		Msg:  fmt.Sprintf("Unable to generate password; Err: %v", err),
		Op:   "kv/setPassword",
	}
}

var (
	userpasswordBucket = []byte("userspasswordv1")
)

var _ influxdb.PasswordsService = (*Service)(nil)

// CompareAndSetPassword checks the password and if they match
// updates to the new password.
func (s *Service) CompareAndSetPassword(ctx context.Context, userID influxdb.ID, old string, new string) error {
	newHash, err := s.generatePasswordHash(new)
	if err != nil {
		return err
	}

	if err := s.compareUserPassword(ctx, userID, old); err != nil {
		return err
	}

	return s.setPasswordHash(ctx, userID, newHash)
}

// SetPassword overrides the password of a known user.
func (s *Service) SetPassword(ctx context.Context, userID influxdb.ID, password string) error {
	hash, err := s.generatePasswordHash(password)
	if err != nil {
		return err
	}

	return s.setPasswordHash(ctx, userID, hash)
}

// ComparePassword checks if the password matches the password recorded.
// Passwords that do not match return errors.
func (s *Service) ComparePassword(ctx context.Context, userID influxdb.ID, password string) error {
	return s.compareUserPassword(ctx, userID, password)
}

func (s *Service) getPasswordHash(ctx context.Context, userID influxdb.ID) ([]byte, error) {
	var passwordHash []byte
	err := s.kv.View(ctx, func(tx Tx) error {
		var err error
		encodedID, err := userID.Encode()
		if err != nil {
			return CorruptUserIDError(userID.String(), err)
		}

		if _, err := s.findUserByID(ctx, tx, userID); err != nil {
			return EIncorrectUser
		}

		b, err := tx.Bucket(userpasswordBucket)
		if err != nil {
			return UnavailablePasswordServiceError(err)
		}

		passwordHash, err = b.Get(encodedID)
		if err != nil {
			return EIncorrectPassword
		}

		return nil
	})

	return passwordHash, err
}

func (s *Service) compareUserPassword(ctx context.Context, userID influxdb.ID, password string) error {
	passwordHash, err := s.getPasswordHash(ctx, userID)
	if err != nil {
		return err
	}

	hasher := s.Hash
	if hasher == nil {
		hasher = &Bcrypt{}
	}

	if err := hasher.CompareHashAndPassword(passwordHash, []byte(password)); err != nil {
		return EIncorrectPassword
	}

	return nil
}

func (s *Service) setPasswordHash(ctx context.Context, userID influxdb.ID, hash []byte) error {
	return s.kv.Update(ctx, func(tx Tx) error {
		return s.setPasswordHashTx(ctx, tx, userID, hash)
	})
}

func (s *Service) setPasswordHashTx(ctx context.Context, tx Tx, userID influxdb.ID, hash []byte) error {
	encodedID, err := userID.Encode()
	if err != nil {
		return CorruptUserIDError(userID.String(), err)
	}

	if _, err := s.findUserByID(ctx, tx, userID); err != nil {
		return EIncorrectUser
	}

	b, err := tx.Bucket(userpasswordBucket)
	if err != nil {
		return UnavailablePasswordServiceError(err)
	}

	if err := b.Put(encodedID, hash); err != nil {
		return UnavailablePasswordServiceError(err)
	}

	return nil
}

func (s *Service) generatePasswordHash(password string) ([]byte, error) {
	if len(password) < MinPasswordLength {
		return nil, EShortPassword
	}

	hasher := s.Hash
	if hasher == nil {
		hasher = &Bcrypt{}
	}
	hash, err := hasher.GenerateFromPassword([]byte(password), DefaultCost)
	if err != nil {
		return nil, InternalPasswordHashError(err)
	}
	return hash, nil
}

// DefaultCost is the cost that will actually be set if a cost below MinCost
// is passed into GenerateFromPassword
var DefaultCost = bcrypt.DefaultCost

// Crypt represents a cryptographic hashing function.
type Crypt interface {
	// CompareHashAndPassword compares a hashed password with its possible plaintext equivalent.
	// Returns nil on success, or an error on failure.
	CompareHashAndPassword(hashedPassword, password []byte) error
	// GenerateFromPassword returns the hash of the password at the given cost.
	// If the cost given is less than MinCost, the cost will be set to DefaultCost, instead.
	GenerateFromPassword(password []byte, cost int) ([]byte, error)
}

var _ Crypt = (*Bcrypt)(nil)

// Bcrypt implements Crypt using golang.org/x/crypto/bcrypt
type Bcrypt struct{}

// CompareHashAndPassword compares a hashed password with its possible plaintext equivalent.
// Returns nil on success, or an error on failure.
func (b *Bcrypt) CompareHashAndPassword(hashedPassword, password []byte) error {
	return bcrypt.CompareHashAndPassword(hashedPassword, password)
}

// GenerateFromPassword returns the hash of the password at the given cost.
// If the cost given is less than MinCost, the cost will be set to DefaultCost, instead.
func (b *Bcrypt) GenerateFromPassword(password []byte, cost int) ([]byte, error) {
	if cost < bcrypt.MinCost {
		cost = DefaultCost
	}
	return bcrypt.GenerateFromPassword(password, cost)
}
