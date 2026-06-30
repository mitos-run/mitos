// Hosted quickstart: the snippet shown in the Go SDK README.
//
// This file is the checked copy of the hosted "Quickstart" hero so the README
// code cannot drift from the real SDK surface. The sdk-examples CI job compiles
// it (go build ./examples/... and go vet), so a renamed or removed method fails
// the build.
//
// It is NOT executed in CI: the hosted default (NewSandboxServer with no base
// URL) talks to https://mitos.run with a real MITOS_API_KEY, which CI does not
// carry. End-to-end execution of the direct path is proven against a real KVM
// sandbox-server by examples/direct in the kvm-test job. Run this one yourself:
//
//	export MITOS_API_KEY="sk-..."
//	go run ./sdk/go/examples/quickstart
package main

import (
	"context"
	"fmt"
	"log"

	mitos "github.com/mitos-run/mitos/sdk/go"
)

func main() {
	ctx := context.Background()
	srv := mitos.NewSandboxServer() // base URL + token resolved from the env

	if _, err := srv.CreateTemplate(ctx, "python"); err != nil {
		log.Fatal(err)
	}
	sb, err := srv.Fork(ctx, "python", "") // empty id -> generated sandbox-<hex>
	if err != nil {
		log.Fatal(err)
	}
	defer sb.Terminate(ctx)

	res, err := sb.Exec(ctx, "echo hi")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Stdout)
}
