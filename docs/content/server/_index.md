---
title: "Server"
linkTitle: "Server"
description: "gr serve: the Bolt wire protocol server for Neo4j-compatible drivers and the HTTP JSON API."
weight: 50
---

`gr serve graph.gr` turns any `.gr` file into a server that any Neo4j driver can connect to over the Bolt wire protocol.
The same graph the library queries is reachable from Python, JavaScript, Java, .NET, and Rust without changing the database file.
