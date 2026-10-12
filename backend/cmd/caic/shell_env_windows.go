// Keeps the inherited environment on Windows, where Unix login shells do not apply.

package main

import "context"

func importShellEnvironment(context.Context) error {
	return nil
}
