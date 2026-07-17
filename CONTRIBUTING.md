# How to contribute

We'd love to accept your patches and contributions to this project. However, please note our current temporary policy below.

> [!IMPORTANT]
> **Temporary Pause on External Contributions**
> 
> AX is currently undergoing a significant architectural redesign, and the codebase is highly unstable. To ensure we can focus on stabilizing the core framework, we are temporarily **pausing the acceptance of external Pull Requests**.
> 
> Please do not open Pull Requests at this time, as they are likely to conflict with ongoing changes and may be closed without review. Instead, we encourage you to:
> - Open an **Issue** to propose feature requests or report bugs.
> - Join discussions on existing issues to share your feedback.
> 
> We will update this guide once the core architecture stabilizes and we are ready to welcome contributions again. Thank you for your understanding and patience!

## Before you begin

### Sign our Contributor License Agreement

All submissions to this project need to follow Google’s [Contributor
License Agreement (CLA)](https://cla.developers.google.com/about), which
covers any original work of authorship included in the submission. This
doesn't prohibit the use of coding assistance tools, including tool-,
AI-, or machine-generated code, as long as these submissions abide by the
CLA's requirements.

You (or your employer) retain the copyright to your contribution; this simply
gives us permission to use and redistribute your contributions as part of the
project.

If you or your current employer have already signed the Google CLA (even if it
was for a different project), you probably don't need to do it again.

Visit <https://cla.developers.google.com/> to see your current agreements or to
sign a new one.

### Review our community guidelines

This project follows
[Google's Open Source Community Guidelines](https://opensource.google/conduct/).

### Code reviews

All submissions, including submissions by project members, require review. We
use GitHub pull requests for this purpose. Consult
[GitHub Help](https://help.github.com/articles/about-pull-requests/) for more
information on using pull requests.

## Contribution workflow

### Generate Protobuf Code

```bash
make proto
```

### Run Tests

```bash
make test
```

### Install the ax CLI

```bash
make install
```

Ensure that $GOPATH/bin is in your $PATH.

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
```

If you have the `GOBIN` environment variable set, it will be installed there instead.
Make sure the installation directory is in your `$PATH`.

To add the default location to your `PATH` for the current session, run:

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
```

### Creating a pull request

First, clone the repo:

```
git clone git@github.com:google/ax.git
```

If you already have cloned the repo locally, make sure that
your main branch is up to date:

```
git checkout main
git pull -q -r origin main
```

Check a new feature branch:

```
git checkout -b my-feature
```

Make edits to files, and test them locally. Add the changes (e.g. git add .) to stage.
Commit the changes once you staged the changes:

```
git commit -m "Describe he changes made"
```

Push the branch to the origin and open a pull request:

```
git push origin my-feature
```

Visit https://github.com/google/ax to open a pull request.


## Troubleshooting

### Outdated table schema

AX is still under heavy development and the database schema is not yet stable. If you encounter errors related to outdated table schemas, you can reset the database by deleting the `eventlog` directory.

An example:

```bash
ax exec --input "hello"

Error: error creating controller: failed to create event log: sqlite_eventlog: create index exec_checkpoint_id: SQL logic error: no such column: checkpoint_id (1)
```

Delete the `eventlog` directory and try again.

```bash
rm -rf ./eventlog
```
