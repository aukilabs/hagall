# Contributing to the Relay Server

Welcome and thank you for considering to contribute to the Relay Server
and related library code.

Please read the following guidelines to ensure the smoothest possible
experience for both yourself and the Auki Labs developers. Thank you.

## Responsible disclosure

If you believe you have found a vulnerability/security issue, please send
details of it to security@aukilabs.com. Please do not open an issue on GitHub
for it or spread info about it publicly.

## Reporting issues

If you think you found a bug, please first try searching for it in
[the project's issue tracker](https://github.com/aukilabs/hagall/issues),
to make sure it has not been reported yet.

Before reporting an issue, please make sure that you are using the latest
version and your configuration is correct. Also, when reporting an issue please
include the following info:

* Relevant logs
* Stack trace
* OS, platform and version (Windows/Linux/macOS, x86/ARM)
* Version of the interpreter, compiler, SDK, runtime environment, package
  manager etc. depending on what seems relevant.
* Can you reliably reproduce the issue?

## Code contributions

If you would like to contribute code to the project, please open a pull request
with your changes. If you did not yet read and sign our
Contributor License Agreement, you will be asked to do so when opening
a pull request.

When contributing code, make sure your code follows the code style/format and
naming conventions of the existing codebase. The Hagall and related code is
trying to use mainly Golang recommended code conventions.

The following lists are taken from
[Microsoft Go Code Review](https://microsoft.github.io/code-with-engineering-playbook/code-reviews/recipes/go/),
with additional items evolved from our team’s requirements.

### Style guide

* [Effective Go](https://golang.org/doc/effective_go.html)
* Directory structure:
  - Following [Golang src directory structure](https://github.com/golang/go/tree/master/src) whenever possible.
  - Packages inside [internal directory](https://go.dev/doc/go1.4#internalpackages) are private within the repo.
  - Exported packages should be put under a `pkg` base directory.
  - Follow [Unofficial Golang Directory Structre](https://github.com/golang-standards/project-layout) if all else failed.


### Checklist

- Does the code handle errors correctly? This includes not throwing away errors with _ assignments and returning errors, instead of in-band error values?
- Does the code follow Go standards for method receiver types?
- Does the code pass values when it should?
- Are interfaces in the code defined in the correct packages?
- Do go-routines in the code have clear lifetimes?
- Is parallelism in the code handled via go-routines and channels with synchronous methods?
- Does the code have meaningful doc comments?
- Does the code have meaningful package comments?
- Does the code use Contexts correctly?
- Do unit tests fail with meaningful messages?
- Does the code use pointers unnecesarily?
- Does the code use deprecated / archived packages?
- Are all inputs coming from user validated?
- Does the pull request / code change adhere to best practices outlined by Auki Labs, Golang, Postgres or other tech stacks we are using?

---

- [Conventional Commit](https://www.conventionalcommits.org/en/v1.0.0/)
- [Golang - Effective Go](https://golang.org/doc/effective_go.html)
- [Golang - Common Code Review Comment](https://github.com/golang/go/wiki/CodeReviewComments)
- [Postgres - Don’t Do This](https://wiki.postgresql.org/wiki/Don%27t_Do_This)
