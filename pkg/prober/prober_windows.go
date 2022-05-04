//go:build windows
// +build windows

package prober

import (
	"crypto/x509"
	"fmt"
	"syscall"
	"unsafe"

	"github.com/sirupsen/logrus"
)

const (
	CRYPT_E_NOT_FOUND = 0x80092004
)

// GetSystemCertPool is a workaround to Windows not having x509.SystemCertPool implemented in < go1.18
// it leverages syscalls to extract system certificates and load them into a new x509.CertPool
// workaround adapted from: https://github.com/golang/go/issues/16736#issuecomment-540373689
// ref: https://docs.microsoft.com/en-us/windows/win32/api/wincrypt/nf-wincrypt-certgetissuercertificatefromstore
// TODO: Test and remove after system-agent is bumped to go1.18+
func GetSystemCertPool(probeName string) (*x509.CertPool, error) {
	logrus.Tracef("[GetSystemCertPoolWindows] building system cert pool for probe (%s)", probeName)
	root, err := syscall.UTF16PtrFromString("Root")
	if err != nil {
		return nil, fmt.Errorf("[GetSystemCertPoolWindows] unable to return UTF16 pointer: %v", syscall.GetLastError())
	}
	if root == nil {
		return nil, fmt.Errorf("[GetSystemCertPoolWindows] UTF16 pointer for Root returned nil: %v", syscall.GetLastError())

	}
	storeHandle, err := syscall.CertOpenSystemStore(0, root)
	if err != nil {
		return nil, fmt.Errorf("[GetSystemCertPoolWindows] unable to open system cert store: %v", syscall.GetLastError())
	}

	var certs []*x509.Certificate
	var cert *syscall.CertContext

	cert, err = syscall.CertEnumCertificatesInStore(storeHandle, cert)
	if err != nil {
		if errno, ok := err.(syscall.Errno); ok {
			if errno == CRYPT_E_NOT_FOUND {
				return nil, fmt.Errorf("[GetSystemCertPoolWindows] no certificate context was found for probe (%s)", probeName)
			}
		}
		return nil, fmt.Errorf("[GetSystemCertPoolWindows] unable to enumerate certs in system cert store for probe (%s): %v", probeName, syscall.GetLastError())
	}
	if cert == nil {
		return nil, fmt.Errorf("[GetSystemCertPoolWindows] certificate context returned from syscall is nil for probe (%s)", probeName)
	}
	// Copy the buf, since ParseCertificate does not create its own copy.
	buf := (*[1 << 20]byte)(unsafe.Pointer(cert.EncodedCert))[:]
	buf2 := make([]byte, cert.Length)
	copy(buf2, buf)
	c, err := x509.ParseCertificate(buf2)
	if err != nil {
		return nil, fmt.Errorf("[GetSystemCertPoolWindows] unable to parse x509 certificate for probe (%s): %v", probeName, err)
	}
	certs = append(certs, c)
	logrus.Debugf("[GetSystemCertPoolWindows] Successfully loaded %d certificates from system cert store for probe (%s)", len(certs), probeName)

	caCertPool := x509.NewCertPool()
	for _, certificate := range certs {
		if !caCertPool.AppendCertsFromPEM(certificate.RawTBSCertificate) {
			return nil, fmt.Errorf("[GetSystemCertPoolWindows] unable to append cert with CN [%s] to system cert pool for probe (%s)", c.Subject.CommonName, probeName)
		}
		logrus.Tracef("[GetSystemCertPoolWindows] successfully appended cert with CN [%s] to system cert pool for probe (%s)", c.Subject.CommonName, probeName)
	}
	logrus.Infof("[GetSystemCertPoolWindows] Successfully loaded %d certificates into system cert pool for probe (%s)", len(certs), probeName)

	return caCertPool, nil
}
