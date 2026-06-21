package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

func ReadSecretMappings(path string) ([]string, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open secret mappings: %w", err)
	}
	defer file.Close()

	var mappings []string
	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), "#", 2)[0])
		if line == "" {
			continue
		}
		guest, host, ok := strings.Cut(line, "=")
		if !ok || !validEnvironmentName(strings.TrimSpace(guest)) ||
			!validEnvironmentName(strings.TrimSpace(host)) {
			return nil, fmt.Errorf(
				"%s:%d: expected GUEST_VARIABLE=HOST_VARIABLE",
				path,
				lineNumber,
			)
		}
		mappings = append(
			mappings,
			strings.TrimSpace(guest)+"="+strings.TrimSpace(host),
		)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read secret mappings: %w", err)
	}
	return mappings, nil
}

func ValidateSecretMappingEnvironment(
	mappings []string,
	lookupEnv func(string) (string, bool),
) error {
	for _, mapping := range mappings {
		guest, host, _ := strings.Cut(mapping, "=")
		if value, ok := lookupEnv(host); !ok || value == "" {
			return fmt.Errorf(
				"host environment variable %q referenced by secret mapping for %q is unset or empty",
				host,
				guest,
			)
		}
	}
	return nil
}

func secretHostEnvironmentKeys(mappings []string) map[string]struct{} {
	result := make(map[string]struct{}, len(mappings))
	for _, mapping := range mappings {
		_, host, _ := strings.Cut(mapping, "=")
		result[host] = struct{}{}
	}
	return result
}

func validEnvironmentName(value string) bool {
	if value == "" || !(value[0] == '_' || value[0] >= 'A' && value[0] <= 'Z' ||
		value[0] >= 'a' && value[0] <= 'z') {
		return false
	}
	for _, character := range value[1:] {
		if character != '_' &&
			(character < 'A' || character > 'Z') &&
			(character < 'a' || character > 'z') &&
			(character < '0' || character > '9') {
			return false
		}
	}
	return true
}
