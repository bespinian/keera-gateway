# Security

## Reporting a problem

Do not open a public issue. Write to us through the
[bespinian contact form](https://bespinian.io/en/contact/#form), so we can fix
it before it is public.

Please include:

- what an attacker can do, and what they need first
- the steps to reproduce it
- the output of `keera version`, or the image tag

We confirm that we got your report, keep you informed while we work on it, and
credit you when the fix is out, unless you would rather stay anonymous.

## Supported versions

Only the latest release gets security fixes.

## Verifying a release

Every release archive and image is signed with build provenance.
To check a download:

```sh
sha256sum --check --ignore-missing SHA256SUMS
gh attestation verify keera_vX.Y.Z_linux_amd64.tar.gz --repo bespinian/keera-gateway \
  --signer-workflow bespinian/keera-gateway/.github/workflows/release-build.yml
```

`--signer-workflow` checks that the release workflow signed the file, not some
other workflow in the repository. Check the image the same way, with
`oci://ghcr.io/bespinian/keera-gateway:<tag>` in place of the file.
