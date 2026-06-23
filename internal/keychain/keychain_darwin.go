//go:build darwin && cgo

package keychain

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>
#include <Security/Security.h>
#include <stdlib.h>
#include <string.h>

static CFMutableDictionaryRef hbQuery(const void *service, CFIndex serviceLen, const void *account, CFIndex accountLen) {
    CFMutableDictionaryRef query = CFDictionaryCreateMutable(NULL, 0, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
    CFStringRef serviceData = CFStringCreateWithBytes(NULL, service, serviceLen, kCFStringEncodingUTF8, false);
    CFStringRef accountData = CFStringCreateWithBytes(NULL, account, accountLen, kCFStringEncodingUTF8, false);
    CFDictionarySetValue(query, kSecClass, kSecClassGenericPassword);
    CFDictionarySetValue(query, kSecAttrService, serviceData);
    CFDictionarySetValue(query, kSecAttrAccount, accountData);
    CFRelease(serviceData);
    CFRelease(accountData);
    return query;
}

static OSStatus hbGet(const void *service, CFIndex serviceLen, const void *account, CFIndex accountLen, void **out, CFIndex *outLen) {
    CFMutableDictionaryRef query = hbQuery(service, serviceLen, account, accountLen);
    CFDictionarySetValue(query, kSecReturnData, kCFBooleanTrue);
    CFDictionarySetValue(query, kSecMatchLimit, kSecMatchLimitOne);
    CFTypeRef result = NULL;
    OSStatus status = SecItemCopyMatching(query, &result);
    CFRelease(query);
    if (status != errSecSuccess) return status;
    CFDataRef data = (CFDataRef)result;
    *outLen = CFDataGetLength(data);
    *out = malloc(*outLen);
    if (*outLen > 0) memcpy(*out, CFDataGetBytePtr(data), *outLen);
    CFRelease(data);
    return errSecSuccess;
}

static OSStatus hbPut(const void *service, CFIndex serviceLen, const void *account, CFIndex accountLen, const void *secret, CFIndex secretLen) {
    CFMutableDictionaryRef query = hbQuery(service, serviceLen, account, accountLen);
    CFDataRef secretData = CFDataCreate(NULL, secret, secretLen);
    CFMutableDictionaryRef update = CFDictionaryCreateMutable(NULL, 0, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
    CFDictionarySetValue(update, kSecValueData, secretData);
    OSStatus status = SecItemUpdate(query, update);
    CFRelease(update);
    if (status == errSecItemNotFound) {
        CFDictionarySetValue(query, kSecValueData, secretData);
        status = SecItemAdd(query, NULL);
    }
    CFRelease(secretData);
    CFRelease(query);
    return status;
}

static OSStatus hbCreate(const void *service, CFIndex serviceLen, const void *account, CFIndex accountLen, const void *secret, CFIndex secretLen) {
    CFMutableDictionaryRef query = hbQuery(service, serviceLen, account, accountLen);
    CFDataRef secretData = CFDataCreate(NULL, secret, secretLen);
    CFDictionarySetValue(query, kSecValueData, secretData);
    CFDictionarySetValue(query, kSecAttrAccessible, kSecAttrAccessibleWhenUnlockedThisDeviceOnly);
    OSStatus status = SecItemAdd(query, NULL);
    CFRelease(secretData);
    CFRelease(query);
    return status;
}

static OSStatus hbDelete(const void *service, CFIndex serviceLen, const void *account, CFIndex accountLen) {
    CFMutableDictionaryRef query = hbQuery(service, serviceLen, account, accountLen);
    OSStatus status = SecItemDelete(query);
    CFRelease(query);
    return status;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"unicode/utf8"
	"unsafe"
)

type KeychainStore struct {
	service []byte
}

func New(service string) (*KeychainStore, error) {
	if service == "" || !utf8.ValidString(service) {
		return nil, errors.New("Keychain service is required")
	}
	return &KeychainStore{service: []byte(service)}, nil
}

func (s *KeychainStore) Get(account string) ([]byte, error) {
	if account == "" || !utf8.ValidString(account) {
		return nil, errors.New("Keychain account is required")
	}
	accountBytes := []byte(account)
	var output unsafe.Pointer
	var outputLength C.CFIndex
	status := C.hbGet(bytesPointer(s.service), C.CFIndex(len(s.service)), bytesPointer(accountBytes), C.CFIndex(len(accountBytes)), &output, &outputLength)
	if status == C.errSecItemNotFound {
		return nil, ErrNotFound
	}
	if status != C.errSecSuccess {
		return nil, fmt.Errorf("read macOS Keychain item: status %d", int(status))
	}
	defer C.free(output)
	return C.GoBytes(output, C.int(outputLength)), nil
}

func (s *KeychainStore) Put(account string, secret []byte) error {
	if account == "" || !utf8.ValidString(account) || len(secret) == 0 {
		return errors.New("Keychain account and secret are required")
	}
	accountBytes := []byte(account)
	status := C.hbPut(bytesPointer(s.service), C.CFIndex(len(s.service)), bytesPointer(accountBytes), C.CFIndex(len(accountBytes)), bytesPointer(secret), C.CFIndex(len(secret)))
	if status != C.errSecSuccess {
		return fmt.Errorf("write macOS Keychain item: status %d", int(status))
	}
	return nil
}

func (s *KeychainStore) Create(account string, secret []byte) (bool, error) {
	if account == "" || !utf8.ValidString(account) || len(secret) == 0 {
		return false, errors.New("Keychain account and secret are required")
	}
	accountBytes := []byte(account)
	status := C.hbCreate(bytesPointer(s.service), C.CFIndex(len(s.service)), bytesPointer(accountBytes), C.CFIndex(len(accountBytes)), bytesPointer(secret), C.CFIndex(len(secret)))
	if status == C.errSecDuplicateItem {
		return false, nil
	}
	if status != C.errSecSuccess {
		return false, fmt.Errorf("create macOS Keychain item: status %d", int(status))
	}
	return true, nil
}

func (s *KeychainStore) Delete(account string) error {
	if account == "" || !utf8.ValidString(account) {
		return errors.New("Keychain account is required")
	}
	accountBytes := []byte(account)
	status := C.hbDelete(bytesPointer(s.service), C.CFIndex(len(s.service)), bytesPointer(accountBytes), C.CFIndex(len(accountBytes)))
	if status == C.errSecItemNotFound {
		return ErrNotFound
	}
	if status != C.errSecSuccess {
		return fmt.Errorf("delete macOS Keychain item: status %d", int(status))
	}
	return nil
}

func bytesPointer(value []byte) unsafe.Pointer {
	return unsafe.Pointer(&value[0])
}
