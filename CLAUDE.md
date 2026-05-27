# Context: Infra (Kubernetes Controller)

@~/claude-context/stacks/go-k8s-controller.md

## Local Specifics

- Go version: 1.22+
- API group: `<mygroup.example.com>`
- Run locally: `make run` (against current kubeconfig context)
- Test: `make test` (uses envtest, no cluster needed)
