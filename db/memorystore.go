package db

import (
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/letsencrypt/pebble/v2/acme"
	"github.com/letsencrypt/pebble/v2/core"
)

// ExistingAccountError is an error type indicating when an operation fails
// because the MatchingAccount has a key conflict.
type ExistingAccountError struct {
	MatchingAccount *core.Account
}

func (e ExistingAccountError) Error() string {
	return fmt.Sprintf("New public key is already in use by account %s", e.MatchingAccount.ID)
}

// Pebble keeps all of its various objects (accounts, orders, etc)
// in-memory, not persisted anywhere. MemoryStore implements this in-memory
// "database"
type MemoryStore struct {
	sync.RWMutex

	accountRand *rand.Rand

	accountsByID map[string]*core.Account

	// Each Accounts's key ID is the hex encoding of a SHA256 sum over its public
	// key bytes.
	accountsByKeyID map[string]*core.Account

	// ordersByIssuedSerial indexes the hex encoding of the certificate's
	// SerialNumber.
	ordersByIssuedSerial map[string]*core.Order
	ordersByID           map[string]*core.Order
	ordersByAccountID    map[string][]*core.Order

	authorizationsByID map[string]*core.Authorization

	challengesByID map[string]*core.Challenge

	certificatesByID        map[string]*core.Certificate
	revokedCertificatesByID map[string]*core.RevokedCertificate

	externalAccountKeysByID map[string][]byte

	blockListByDomain [][]string
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		accountRand:             rand.New(rand.NewSource(time.Now().UnixNano())),
		accountsByID:            make(map[string]*core.Account),
		accountsByKeyID:         make(map[string]*core.Account),
		ordersByIssuedSerial:    make(map[string]*core.Order),
		ordersByID:              make(map[string]*core.Order),
		ordersByAccountID:       make(map[string][]*core.Order),
		authorizationsByID:      make(map[string]*core.Authorization),
		challengesByID:          make(map[string]*core.Challenge),
		certificatesByID:        make(map[string]*core.Certificate),
		revokedCertificatesByID: make(map[string]*core.RevokedCertificate),
		externalAccountKeysByID: make(map[string][]byte),
		blockListByDomain:       make([][]string, 0),
	}
}

func (m *MemoryStore) GetAccountByID(id string) *core.Account {
	m.RLock()
	defer m.RUnlock()
	return m.accountsByID[id]
}

func (m *MemoryStore) GetAccountByKey(key crypto.PublicKey) (*core.Account, error) {
	keyID, err := keyToID(key)
	if err != nil {
		return nil, err
	}

	m.RLock()
	defer m.RUnlock()
	return m.accountsByKeyID[keyID], nil
}

// UpdateReplacedOrder takes a serial and marks a parent order as
// replaced/not-replaced or returns an error.
//
// We intentionally don't Lock the database inside this method because the inner
// GetOrderByIssuedSerial which is used elsewhere does an RLock which would
// hang.
func (m *MemoryStore) UpdateReplacedOrder(serial string, shouldBeReplaced bool) error {
	if serial == "" {
		return acme.InternalErrorProblem("no serial provided")
	}

	originalOrder, err := m.GetOrderByIssuedSerial(serial)
	if err != nil {
		return acme.InternalErrorProblem(fmt.Sprintf("could not find an order for the given certificate: %s", err))
	}
	originalOrder.Lock()
	defer originalOrder.Unlock()
	originalOrder.IsReplaced = shouldBeReplaced

	return nil
}

// Note that this function should *NOT* be used for key changes. It assumes
// the public key associated to the account does not change. Use ChangeAccountKey
// to change the account's public key.
func (m *MemoryStore) UpdateAccountByID(id string, acct *core.Account) error {
	m.Lock()
	defer m.Unlock()
	if m.accountsByID[id] == nil {
		return fmt.Errorf("account with ID %q does not exist", id)
	}
	keyID, err := keyToID(acct.Key)
	if err != nil {
		return err
	}
	m.accountsByID[id] = acct
	m.accountsByKeyID[keyID] = acct
	return nil
}

func (m *MemoryStore) AddAccount(acct *core.Account) (int, error) {
	m.Lock()
	defer m.Unlock()

	if acct.Key == nil {
		return 0, errors.New("account must not have a nil Key")
	}

	keyID, err := keyToID(acct.Key)
	if err != nil {
		return 0, err
	}

	var acctID string
	for {
		acctID = strconv.FormatInt(m.accountRand.Int63(), 16)
		if _, present := m.accountsByID[acctID]; !present {
			break
		}
	}

	if _, present := m.accountsByKeyID[keyID]; present {
		return 0, errors.New("account with key already exists")
	}

	acct.ID = acctID
	m.accountsByID[acctID] = acct
	m.accountsByKeyID[keyID] = acct
	return len(m.accountsByID), nil
}

func (m *MemoryStore) ChangeAccountKey(acct *core.Account, newKey *jose.JSONWebKey) error {
	m.Lock()
	defer m.Unlock()

	oldKeyID, err := keyToID(acct.Key)
	if err != nil {
		return err
	}

	newKeyID, err := keyToID(newKey)
	if err != nil {
		return err
	}

	if otherAccount, present := m.accountsByKeyID[newKeyID]; present {
		return ExistingAccountError{otherAccount}
	}

	delete(m.accountsByKeyID, oldKeyID)
	acct.Key = newKey
	m.accountsByKeyID[newKeyID] = acct
	m.accountsByID[acct.ID] = acct
	return nil
}

func (m *MemoryStore) AddOrder(order *core.Order) (int, error) {
	m.Lock()
	defer m.Unlock()

	order.RLock()
	orderID := order.ID
	accountID := order.AccountID
	order.RUnlock()
	if len(orderID) == 0 {
		return 0, errors.New("order must have a non-empty ID to add to MemoryStore")
	}

	if _, present := m.ordersByID[orderID]; present {
		return 0, fmt.Errorf("order %q already exists", orderID)
	}

	var ordersByAccountID []*core.Order
	var present bool
	if ordersByAccountID, present = m.ordersByAccountID[accountID]; !present {
		ordersByAccountID = make([]*core.Order, 0)
	}
	m.ordersByAccountID[accountID] = append(ordersByAccountID, order)

	m.ordersByID[orderID] = order
	return len(m.ordersByID), nil
}

func (m *MemoryStore) AddOrderByIssuedSerial(order *core.Order) error {
	m.Lock()
	defer m.Unlock()

	if order.CertificateObject == nil {
		return errors.New("order must have non-empty CertificateObject")
	}

	m.ordersByIssuedSerial[order.CertificateObject.ID] = order

	return nil
}

func (m *MemoryStore) GetOrderByID(id string) *core.Order {
	m.RLock()
	defer m.RUnlock()

	if order, ok := m.ordersByID[id]; ok {
		orderStatus, err := order.GetStatus()
		if err != nil {
			panic(err)
		}
		order.Lock()
		defer order.Unlock()
		order.Status = orderStatus
		return order
	}
	return nil
}

// GetOrderByIssuedSerial returns the order that resulted in the given certificate
// serial. If no such order exists, an error will be returned.
func (m *MemoryStore) GetOrderByIssuedSerial(serial string) (*core.Order, error) {
	m.RLock()
	defer m.RUnlock()

	order, ok := m.ordersByIssuedSerial[serial]
	if !ok {
		return nil, errors.New("could not find order resulting in the given certificate serial number")
	}

	return order, nil
}

func (m *MemoryStore) GetOrdersByAccountID(accountID string) []*core.Order {
	m.RLock()
	defer m.RUnlock()

	if orders, ok := m.ordersByAccountID[accountID]; ok {
		for _, order := range orders {
			orderStatus, err := order.GetStatus()
			if err != nil {
				panic(err)
			}
			order.Lock()
			defer order.Unlock()
			order.Status = orderStatus
		}
		return orders
	}
	return nil
}

func (m *MemoryStore) AddAuthorization(authz *core.Authorization) (int, error) {
	m.Lock()
	defer m.Unlock()

	authz.RLock()
	authzID := authz.ID
	if len(authzID) == 0 {
		return 0, errors.New("authz must have a non-empty ID to add to MemoryStore")
	}
	authz.RUnlock()

	if _, present := m.authorizationsByID[authzID]; present {
		return 0, fmt.Errorf("authz %q already exists", authzID)
	}

	m.authorizationsByID[authzID] = authz
	return len(m.authorizationsByID), nil
}

func (m *MemoryStore) GetAuthorizationByID(id string) *core.Authorization {
	m.RLock()
	defer m.RUnlock()
	return m.authorizationsByID[id]
}

// FindValidAuthorization fetches the first, if any, valid and unexpired authorization for the
// provided identifier, from the ACME account matching accountID.
func (m *MemoryStore) FindValidAuthorization(accountID string, identifier acme.Identifier) *core.Authorization {
	m.RLock()
	defer m.RUnlock()
	for _, authz := range m.authorizationsByID {
		// Lock is needed as Authorizations can be mutated outside the scope of the MemoryStore lock.
		// Racey code path is exercised through the test in va/va_test.go, which should be considered
		// for removal in the event that there's a by-value refactoring of MemoryStore.
		authz.RLock()
		if authz.Status == acme.StatusValid && identifier.Equals(authz.Identifier) &&
			authz.Order != nil && authz.Order.AccountID == accountID &&
			authz.ExpiresDate.After(time.Now()) {
			authz.RUnlock()
			return authz
		}
		authz.RUnlock()
	}
	return nil
}

func (m *MemoryStore) AddChallenge(chal *core.Challenge) (int, error) {
	m.Lock()
	defer m.Unlock()

	chal.RLock()
	chalID := chal.ID
	chal.RUnlock()
	if len(chalID) == 0 {
		return 0, errors.New("challenge must have a non-empty ID to add to MemoryStore")
	}

	if _, present := m.challengesByID[chalID]; present {
		return 0, fmt.Errorf("challenge %q already exists", chalID)
	}

	m.challengesByID[chalID] = chal
	return len(m.challengesByID), nil
}

func (m *MemoryStore) GetChallengeByID(id string) *core.Challenge {
	m.RLock()
	defer m.RUnlock()
	return m.challengesByID[id]
}

func (m *MemoryStore) AddCertificate(cert *core.Certificate) (int, error) {
	m.Lock()
	defer m.Unlock()

	certID := cert.ID
	if len(certID) == 0 {
		return 0, errors.New("cert must have a non-empty ID to add to MemoryStore")
	}

	if _, present := m.certificatesByID[certID]; present {
		return 0, fmt.Errorf("cert %q already exists", certID)
	}
	if _, present := m.revokedCertificatesByID[certID]; present {
		return 0, fmt.Errorf("cert %q already exists (and is revoked)", certID)
	}

	m.certificatesByID[certID] = cert
	return len(m.certificatesByID), nil
}

func (m *MemoryStore) GetCertificateByID(id string) *core.Certificate {
	m.RLock()
	defer m.RUnlock()
	return m.certificatesByID[id]
}

// GetCertificateByDER loops over all certificates to find the one that matches the provided DER bytes.
// This method is linear and it's not optimized to give you a quick response.
func (m *MemoryStore) GetCertificateByDER(der []byte) *core.Certificate {
	m.RLock()
	defer m.RUnlock()
	for _, c := range m.certificatesByID {
		if reflect.DeepEqual(c.DER, der) {
			return c
		}
	}

	return nil
}

// GetCertificateByDER loops over all revoked certificates to find the one that matches the provided
// DER bytes. This method is linear and it's not optimized to give you a quick response.
func (m *MemoryStore) GetRevokedCertificateByDER(der []byte) *core.RevokedCertificate {
	m.RLock()
	defer m.RUnlock()
	for _, c := range m.revokedCertificatesByID {
		if reflect.DeepEqual(c.Certificate.DER, der) {
			return c
		}
	}

	return nil
}

func (m *MemoryStore) RevokeCertificate(cert *core.RevokedCertificate) {
	m.Lock()
	defer m.Unlock()
	m.revokedCertificatesByID[cert.Certificate.ID] = cert
}

/*
 * keyToID produces a string with the hex representation of the SHA256 digest
 * over a provided public key. We use this to associate public keys to
 * acme.Account objects, and to ensure every account has a unique public key.
 */
func keyToID(key crypto.PublicKey) (string, error) {
	switch t := key.(type) {
	case *jose.JSONWebKey:
		if t == nil {
			return "", errors.New("cannot compute ID of nil key")
		}
		return keyToID(t.Key)
	case jose.JSONWebKey:
		return keyToID(t.Key)
	default:
		keyDER, err := x509.MarshalPKIXPublicKey(key)
		if err != nil {
			return "", err
		}
		spkiDigest := sha256.Sum256(keyDER)
		return hex.EncodeToString(spkiDigest[:]), nil
	}
}

// GetCertificateBySerial loops over all certificates to find the one that matches the provided
// serial number. This method is linear and it's not optimized to give you a quick response.
func (m *MemoryStore) GetCertificateBySerial(serialNumber *big.Int) *core.Certificate {
	m.RLock()
	defer m.RUnlock()
	for _, c := range m.certificatesByID {
		if serialNumber.Cmp(c.Cert.SerialNumber) == 0 {
			return c
		}
	}

	return nil
}

// GetRevokedCertificateBySerial loops over all revoked certificates to find the one that matches the
// provided serial number. This method is linear and it's not optimized to give you a quick
// response.
func (m *MemoryStore) GetRevokedCertificateBySerial(serialNumber *big.Int) *core.RevokedCertificate {
	m.RLock()
	defer m.RUnlock()
	for _, c := range m.revokedCertificatesByID {
		if serialNumber.Cmp(c.Certificate.Cert.SerialNumber) == 0 {
			return c
		}
	}

	return nil
}

// AddExternalAccountKeyByID will add the base64 URL encoded key to the memory
// store with the key ID as its index. This will store the key value in its
// unencoded, raw form.
func (m *MemoryStore) AddExternalAccountKeyByID(keyID, key string) error {
	if len(key) == 0 || len(keyID) == 0 {
		return errors.New("key ID and key must not be empty")
	}

	keyDecoded, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil {
		return fmt.Errorf("failed to decode base64 URL encoded key %q: %w", key, err)
	}

	m.Lock()
	defer m.Unlock()

	if _, ok := m.externalAccountKeysByID[keyID]; ok {
		return fmt.Errorf("key ID %q is already present", keyID)
	}

	m.externalAccountKeysByID[keyID] = keyDecoded

	return nil
}

// GetExternalAccountKeyByID will return the raw, base64 URL unencoded key
// value by its key ID pair.
func (m *MemoryStore) GetExtenalAccountKeyByID(keyID string) ([]byte, bool) {
	m.RLock()
	defer m.RUnlock()
	key, ok := m.externalAccountKeysByID[keyID]
	return key, ok
}

// AddBlockedDomain will add the domain name to the block list
func (m *MemoryStore) AddBlockedDomain(name string) error {
	if len(name) == 0 {
		return errors.New("domain name must not be empty")
	}

	domainParts := strings.Split(name, ".")

	// reversing the order
	for i, j := 0, len(domainParts)-1; i < j; i, j = i+1, j-1 {
		domainParts[i], domainParts[j] = domainParts[j], domainParts[i]
	}

	m.Lock()
	defer m.Unlock()
	m.blockListByDomain = append(m.blockListByDomain, domainParts)

	return nil
}

// IsDomainBlocked will return true if a domain is on the block list
func (m *MemoryStore) IsDomainBlocked(name string) bool {
	domainParts := strings.Split(name, ".")

	// reversing the order
	for i, j := 0, len(domainParts)-1; i < j; i, j = i+1, j-1 {
		domainParts[i], domainParts[j] = domainParts[j], domainParts[i]
	}

	m.RLock()
	defer m.RUnlock()
	for _, blockedParts := range m.blockListByDomain {
		isMatch := true
		for i := range blockedParts {
			if blockedParts[i] != domainParts[i] {
				isMatch = false
				break
			}
		}
		if isMatch {
			return true
		}
	}

	return false
}

// SetARIResponse looks up a certificate by serial number and sets its ARI response field
func (m *MemoryStore) SetARIResponse(serial *big.Int, ariResponse string) error {
	m.Lock()
	defer m.Unlock()

	for _, cert := range m.certificatesByID {
		if cert.Cert.SerialNumber.Cmp(serial) == 0 {
			cert.ARIResponse = ariResponse
			return nil
		}
	}

	// Certificate not found
	return fmt.Errorf("certificate with serial number %s not found", serial.String())
}
