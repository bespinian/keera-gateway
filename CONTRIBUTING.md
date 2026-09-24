# Contributing to Keera

Thank you for helping. This page explains how to send a change and what you
agree to when you do.

## Before you start

- **Found a bug?** Open an issue. Say what you did, what you expected and what
  happened instead. Include the output of `keera version`.
- **Found a security problem?** Do not open an issue. Write to us through the
  [bespinian contact form](https://bespinian.io/en/contact/#form) instead, so it
  can be fixed before it is public.
- **Planning a larger change?** Open an issue first and describe it, so nobody
  spends a week on something that will not be merged.

## Making a change

[docs/run-locally.md](docs/run-locally.md) shows how to build and run Keera on
your machine. In short:

```sh
go mod vendor   # once, after cloning
make dev        # the gateway and its backends, rebuilt on every save
make check      # tests, vet and formatting - what CI runs
```

Run `make check` before you open a pull request. If your change touches the
database, Redis or rate limiting, run `make test-integration` as well.

Follow the existing style:

- Format with `gofmt`. Use the standard library; do not add a dependency without
  asking first.
- Comments explain why the code is the way it is, not what it does.
- Tests live next to the code they test.
- Never log a secret, and never store a key in plaintext.
- When behaviour changes, update the matching page in `docs/`.
- Keep comments, docs and messages short and in plain language.

Write commit messages in the imperative: "Add size router", not "Added size
router".

## The Contributor Licence Agreement

Keera is published under the [Keera Community Licence](LICENSE), and bespinian
GmbH also offers it under commercial licences. To do both with your code,
bespinian needs your permission. The agreement below gives it. You keep the
copyright in your work.

**To agree, write this line in the description of your pull request:**

> I have read the Keera Contributor Licence Agreement v1.0 and I agree to it.

We cannot merge a pull request without it. You only need to agree once for all
your future contributions, but please repeat the line in each pull request so
it is easy to check.

If you contribute as part of your job, your employer may own what you write.
Make sure they allow you to agree to this before you do.

### Keera Contributor Licence Agreement v1.0

This agreement is between you and **bespinian GmbH**, Switzerland
("bespinian"). It applies to every Contribution you submit to Keera.

**1. Definitions.** "You" means the person or legal entity agreeing to this
agreement. "Contribution" means any work of authorship - code, documentation or
anything else - that you intentionally submit to bespinian for inclusion in
Keera, for example through a pull request. "Keera" means the software bespinian
publishes in this repository and any work based on it.

**2. You keep ownership.** This agreement does not transfer ownership of your
Contribution to bespinian. You may use your Contribution in any other way you
like.

**3. Copyright licence.** You grant bespinian a perpetual, worldwide,
non-exclusive, royalty-free, irrevocable, transferable and sublicensable licence
to use, reproduce, modify, prepare derivative works from, publicly display,
publicly perform and distribute your Contribution and works based on it. This
includes the right to license your Contribution, as part of Keera or on its
own, under the Keera Community Licence, under commercial licences, or under any
other licence terms.

**4. Patent licence.** You grant bespinian, and everyone who receives Keera from
bespinian, a perpetual, worldwide, non-exclusive, royalty-free and irrevocable
licence to make, use, sell, offer to sell, import and otherwise transfer your
Contribution and Keera. This licence covers only the patent claims you can
license that your Contribution, alone or combined with Keera, necessarily
infringes.

**5. Moral rights.** To the extent the law allows, you agree not to assert moral
rights in your Contribution against bespinian or anyone who receives Keera from
bespinian, as long as the use is within the licences above.

**6. What you promise.** You promise that:

- each Contribution is your original work, or you have the right to submit it
  under this agreement;
- if your employer or anyone else has rights in your Contribution, they have
  allowed you to submit it under this agreement, or have waived their rights;
- as far as you know, your Contribution does not infringe anyone else's rights.

**7. Other people's work.** If part of a Contribution is not your own work, say
so in the pull request. Name where it comes from and the licence it is under.
Do not submit it as your own.

**8. No obligation.** bespinian does not have to use your Contribution. If it
does, it may change it.

**9. No warranty.** Unless you agree otherwise in writing, you provide your
Contribution "as is", without warranties of any kind.

**10. Keep us informed.** If anything you promised in section 6 stops being true,
tell bespinian through the [bespinian contact form](https://bespinian.io/en/contact/#form).

**11. Law and courts.** This agreement is governed by the laws of Switzerland,
excluding its conflict-of-law rules. The courts of Bern, Switzerland have
exclusive jurisdiction over disputes arising from it.
