// Package agent is a shim of the kojo core package of the same name.
//
// An extension is a separate module and cannot import kojo's internal
// packages, so the types, helpers and sentinel errors the Slack bot uses
// are replicated here. JSON tags, validation rules and user-visible
// strings are copied from core and must stay in step with it: the structs
// cross the ext-chat wire, and the bot compares the sentinels with
// errors.Is after the HTTP client has mapped kojo's error codes back.
//
// Only what the bot needs is here. Fields of core structs that are
// in-process plumbing (peer routing, attachment ownership, WebUI staging)
// are intentionally absent.
package agent
