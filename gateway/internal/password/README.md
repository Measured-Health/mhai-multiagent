# Breached-password list

`breached.txt.gz` is the gzipped text of the 100,000 most common passwords
from the Xato "10 million passwords" corpus, as published in SecLists:

- File: `Passwords/Common-Credentials/xato-net-10-million-passwords-100000.txt`
- Repository: https://github.com/danielmiessler/SecLists
- Commit: `145f1f45ba7bc5fd6b3966d3a79bf95654deb20e`
- Licence: MIT (Copyright (c) 2018 Daniel Miessler), see the repository's `LICENSE`
- SHA-256 of the uncompressed file: `1472aafa2561df5e3293aee252aee3ca660c12b399a283cf808bb01b39be388b`
- Entries: 100,000, one per line, in descending frequency, unmodified
  (`gzip -9n`).

The list is embedded in the gateway binary. `password.Check` compares
lowercased, so case variants of a listed password are refused too. To refresh
it, download the same file from a newer SecLists commit, gzip it, and update
the commit and hash above. `TestBreachedListSize` fails if it drops below
100,000 lines.
