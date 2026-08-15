package thalovant

// Version is the module release this package was built from, and the single
// source of truth for every user agent the SDK sends. The VERSION file at the
// repository root is the release pipeline's copy of the same number;
// TestVersionMatchesVersionFile keeps the two in step.
//
// Never hard-code a version inside a user-agent literal anywhere else:
// TestNoSourceFileHardCodesAUserAgentVersion rejects it.
const Version = "0.3.9"

// userAgentProduct is the product token shared by the data-plane and
// control-plane user agents.
const userAgentProduct = "ThalovantGoSDK"

// userAgent is what both DefaultUserAgent and DefaultControlUserAgent resolve
// to. Concatenating untyped string constants keeps it a compile-time constant,
// so the exported user agents stay usable in constant expressions.
const userAgent = userAgentProduct + "/" + Version
