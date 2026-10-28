# Breached-password list

`breached.txt.gz` is the gzipped text of every entry of 12 or more characters
in the Xato "10 million passwords" corpus, as published in SecLists. Shorter
entries are left out because the 12-character minimum already refuses them.

- Source file: `Passwords/Common-Credentials/xato-net-10-million-passwords.txt`
  (5,189,454 lines; SHA-256 `c95989fecfae9c91aa9f5c730344d5cabd6335bb094c9c60aba16bf1742a72fe`)
- Repository: https://github.com/danielmiessler/SecLists
- Commit: `145f1f45ba7bc5fd6b3966d3a79bf95654deb20e`
- Licence: MIT (Copyright (c) 2018 Daniel Miessler), see the repository's `LICENSE`
- Filter: keep lines of 12 or more characters (code points), drop later
  duplicates that differ only by case, keep the source's frequency order.
- Result: **515,286 entries**, one per line; SHA-256 of the uncompressed
  result `f3ea7446fc2373ab3b7c79ffc88bb0bdf2a01d5f48789a3e68d234d2d11ee8c1`
  (`gzip -9n`, about 3.8 MB).

The list is embedded in the gateway binary. `password.Check` compares
lowercased, so case variants of a listed password are refused too. To refresh
it, redo the filter on a newer SecLists commit and update the commit, count
and hashes above. `TestBreachedListSize` fails if the list drops below
100,000 entries or holds one shorter than 12 characters.
