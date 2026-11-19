//go:build !windows

package relay

import "errors"

func InstallService(serviceName, displayName string, extraArgs []string) error {
	return errors.New("los servicios de Windows solo están disponibles en Windows")
}

func UninstallService(serviceName string) error {
	return errors.New("los servicios de Windows solo están disponibles en Windows")
}

func RunAsService(serviceName string, worker func(stopCh <-chan struct{})) error {
	return errors.New("los servicios de Windows solo están disponibles en Windows")
}
