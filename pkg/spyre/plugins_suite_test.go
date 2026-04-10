/*
 * +-------------------------------------------------------------------+
 * | Copyright (c) 2025, 2026 IBM Corp.                                |
 * | SPDX-License-Identifier: Apache-2.0                               |
 * +-------------------------------------------------------------------+
 */

package spyre_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	spyreclient "github.com/ibm-aiu/spyre-operator/pkg/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func TestPlugins(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Plugins Suite")
}

var cfg *rest.Config
var k8sClient client.Client
var testEnv *envtest.Environment
var spyreClient *spyreclient.SpyreClient
var logBuf bytes.Buffer

var _ = BeforeSuite(func() {

	var err error

	By("bootstrapping test environment")
	crdPath := filepath.Join("..", "..", "config", "crd", "external")
	_, err = os.Stat(crdPath)
	Expect(err).To(
		BeNil(),
		"%v not exist; spyre-operator must exists the same directory of the device plugin code",
		crdPath)
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{crdPath},
		ErrorIfCRDPathMissing: true,
	}

	// cfg is defined in this file globally.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	// create namespace "test"
	k8sClient, err = client.New(cfg, client.Options{})
	Expect(err).NotTo(HaveOccurred())
	ns := &corev1.Namespace{}
	ns.Name = "test"
	err = k8sClient.Create(context.Background(), ns)
	Expect(err).NotTo(HaveOccurred())

	/*
		spyreClient, err = spyreclient.NewClient(ctx, cfg)
		Expect(err).To(BeNil())
	*/
	GinkgoWriter.TeeTo(&logBuf)

	opts := zap.Options{
		Development: true,
		Level:       zapcore.Level(-1),
		TimeEncoder: zapcore.ISO8601TimeEncoder,
		DestWriter:  GinkgoWriter,
	}
	zlogger := zap.New(zap.UseFlagOptions(&opts))
	logf.SetLogger(zlogger)
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})
