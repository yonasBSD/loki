package validation

import (
	"slices"
	"time"
)

// SmallestPositiveIntPerTenant is returning the minimal positive value of the
// supplied limit function for all given tenants.
func SmallestPositiveIntPerTenant(tenantIDs []string, f func(string) int) int {
	var result *int
	for _, tenantID := range tenantIDs {
		v := f(tenantID)
		if result == nil || v < *result {
			result = &v
		}
	}
	if result == nil {
		return 0
	}
	return *result
}

// SmallestPositiveNonZeroIntPerTenant is returning the minimal positive and
// non-zero value of the supplied limit function for all given tenants. In many
// limits a value of 0 means unlimited so the method will return 0 only if all
// inputs have a limit of 0 or an empty tenant list is given.
func SmallestPositiveNonZeroIntPerTenant(tenantIDs []string, f func(string) int) int {
	var result *int
	for _, tenantID := range tenantIDs {
		v := f(tenantID)
		if v > 0 && (result == nil || v < *result) {
			result = &v
		}
	}
	if result == nil {
		return 0
	}
	return *result
}

// SmallestPositiveNonZeroDurationPerTenant is returning the minimal positive
// and non-zero value of the supplied limit function for all given tenants. In
// many limits a value of 0 means unlimited so the method will return 0 only if
// all inputs have a limit of 0 or an empty tenant list is given.
func SmallestPositiveNonZeroDurationPerTenant(tenantIDs []string, f func(string) time.Duration) time.Duration {
	var result *time.Duration
	for _, tenantID := range tenantIDs {
		v := f(tenantID)
		if v > 0 && (result == nil || v < *result) {
			result = &v
		}
	}
	if result == nil {
		return 0
	}
	return *result
}

// MaxDurationPerTenant is returning the maximum duration per tenant. Without
// tenants given it will return a time.Duration(0).
func MaxDurationPerTenant(tenantIDs []string, f func(string) time.Duration) time.Duration {
	result := time.Duration(0)
	for _, tenantID := range tenantIDs {
		v := f(tenantID)
		if v > result {
			result = v
		}
	}
	return result
}

// MaxDurationOrZeroPerTenant is returning the maximum duration per tenant or zero if one tenant has time.Duration(0).
func MaxDurationOrZeroPerTenant(tenantIDs []string, f func(string) time.Duration) time.Duration {
	var result *time.Duration
	for _, tenantID := range tenantIDs {
		v := f(tenantID)
		if v == 0 {
			return v
		}

		if v > 0 && (result == nil || v > *result) {
			result = &v
		}
	}
	if result == nil {
		return 0
	}
	return *result
}

// IntersectionPerTenant is returning the intersection of feature flags. This is useful to determine the minimal
// feature set supported.
func IntersectionPerTenant(tenantIDs []string, f func(string) []string) []string {
	var result []string
	for _, tenantID := range tenantIDs {
		v := f(tenantID)
		slices.Sort(v)
		if result == nil {
			result = v
			continue
		}
		var updatedResult []string
		for i, j := 0, 0; i < len(result) && j < len(v); {
			if result[i] == v[j] {
				updatedResult = append(updatedResult, result[i])
				i++
				j++
			} else if result[i] < v[j] {
				i++
			} else {
				j++
			}
		}
		result = updatedResult
	}
	return result
}
