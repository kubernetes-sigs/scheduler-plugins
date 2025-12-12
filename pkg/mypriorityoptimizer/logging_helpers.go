// logging_helpers.go
package mypriorityoptimizer

// -------------------------
// msg
// -------------------------

// msg formats a log message by combining the messenger and the message.
// CHECKED
func msg(messenger string, message string) string {
	return messenger + ": " + message
}
