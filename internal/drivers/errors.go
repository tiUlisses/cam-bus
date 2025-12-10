// internal/drivers/errors.go
package drivers

import "errors"

var ErrDriverNotFound = errors.New("no driver registered for this manufacturer/model")
