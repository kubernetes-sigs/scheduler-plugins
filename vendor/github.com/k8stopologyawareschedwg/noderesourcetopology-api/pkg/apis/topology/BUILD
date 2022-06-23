load("@io_bazel_rules_go//go:def.bzl", "go_library")

go_library(
    name = "go_default_library",
    srcs = ["register.go"],
    importmap = "k8s.io/kubernetes/vendor/github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology",
    importpath = "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology",
    visibility = ["//visibility:public"],
)

filegroup(
    name = "package-srcs",
    srcs = glob(["**"]),
    tags = ["automanaged"],
    visibility = ["//visibility:private"],
)

filegroup(
    name = "all-srcs",
    srcs = [
        ":package-srcs",
        "//github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha1:all-srcs",
    ],
    tags = ["automanaged"],
    visibility = ["//visibility:public"],
)
