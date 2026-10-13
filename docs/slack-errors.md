# Slack error messages

How the bot reports a failed turn in Slack.

Slack displays backend startup errors and terminal `error` / failed `done` diagnostics in the originating thread. If an answer was partially produced, the final answer retains that text and posts a separate failure notice. An error is not suppressed by `[[NO_REPLY]]`. User-requested cancellation keeps the existing stop behavior.

For an existing goal, the notice suggests a normal reply or `!goal resume`; replacing the goal requires `!goal clear` followed by `!goal <objective>`.

Diagnostics are limited to 1,200 Unicode characters, escaped for Slack markup, and common credential patterns (Bearer/Basic credentials, URL userinfo, named secret values, and common API tokens) are redacted. This is defense in depth, not a guarantee for arbitrary secret formats: backends should not include secrets in errors.

Unexpected stream closure is also reported as a failure. If no detail is available, Slack explicitly says that the operation failed without an error detail. Slack delivery failures still use the existing delivery-failure handling; backend error visibility cannot overcome Slack API/network outages.
