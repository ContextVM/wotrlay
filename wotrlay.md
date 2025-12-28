# Wotrlay: moderation as a bucket

Wotrlay is a proof-of-concept relay that explores a different approach to moderation: treat it as resource allocation, and let a community's web-of-trust influence who gets more publishing capacity.

Wotrlay is built in Go using the Rely framework, and it is heavily inspired by the WoT example in that repository. This project builds on that foundation and explores a different moderation posture, especially around progressive rate limiting, auth, and rank integration.

For ranking, Wotrlay integrates Relatr, a service that computes a per-pubkey score. Relatr is reached through ContextVM, which means the ranking service isn't a HTTP endpoint, it's a Nostr-native request/response interaction.

This post is part of a larger series. Next, we'll publish a practical article about building CVM clients without SDKs or dependencies—just keys, events, and relays, the code of project is a demonstration of this as well. Here, we'll walk through how Wotrlay works: the bucket model, how rank drives permissions, the newcomer onboarding path, and why ContextVM makes the ranking service swappable without rebuilding the relay.

BTW We're running a public instance at wss://wotr.relatr.xyz that anyone can connect to and test.

The core idea: a progressive bucket

Think of a relay like a small city with limited infrastructure: roads (bandwidth), storage (disk), and staff time (moderation cost). If anyone can dump unlimited load into that city, the city collapses. But if every newcomer must ask permission, you've recreated the velvet-rope club model.

Wotrlay tries a different mechanism: each pubkey gets a "bucket" of publishing capacity.

When you publish, you spend capacity. Over time, the bucket refills. The refill speed—and how much you can burst in a short time—depends on your rank.

That's the progressive part: low-rank identities can still speak, but slowly. Higher-rank identities can publish at a higher pace, without turning the relay into an unbounded sink for spam.

Rank is dynamic, not a badge

In Nostr, there is no global reputation scoreboard that everyone agrees on. Any meaningful ranking has to be contextual: computed from some perspective, using some signals, with some trade-offs.

Relatr embraces this idea. It computes scores for pubkeys from a chosen "source" perspective using social graph proximity plus additional validations. Different communities can run different instances and get different outcomes.

Wotrlay takes that score and answers a narrower, more practical question:

How much publishing capacity should this pubkey receive here?

What makes this approach interesting is how it rewards organic participation. When a newcomer starts publishing and interacting with others, they begin to form connections in the social graph. As those connections grow and the pubkey becomes part of more relationships, the rank naturally rises through network effects. There's no manual approval process or gatekeeper—just the emergent result of participation.

The policy in plain language

Wotrlay turns rank into three permissions.

First: what you can publish.

Below a configurable mid-rank threshold, Wotrlay only accepts basic note events (Kind 1). The goal isn't to moralize about content. It's to reduce the attack surface. Many spam and indexing attacks rely on flooding specialized event kinds; limiting low-rank keys to the simplest kind is a cheap, effective pressure valve.

Second: how fast you can publish.

Instead of a "daily reset" quota that encourages people (and bots) to dump everything in a burst, Wotrlay refills capacity smoothly over time. If you publish steadily, you're steadily allowed. If you try to flood, you hit the bucket wall.

Third: how much you can burst.

Even high-rank keys don't get infinite burst. Wotrlay deliberately caps short-term bursts while still allowing natural behavior—posting threads, coming back online, syncing after downtime.

The newcomer path

Every open relay has a cold start dilemma.

If you give unknown identities too much capacity, spam scales cheaply. If you give them zero capacity, the relay stops being permissionless in practice.

Wotrlay sets a minimal baseline: a newcomer with a rank of 0 can still publish a small amount immediately, one note per day (enough to appear and participate). Meanwhile, the relay begins rank computation in the background, and its bucket refills progressively. As the public key gains visibility and builds real relationships, its rank can rise, and the bucket refills faster. Notice that a rank of 0 is very unlikely for public keys that have already been participating in Nostr for some time.

That "progression" is the main point: instead of flipping between "blocked" and "unlimited," publishing becomes a gradient. 

Why this avoids auth friction

Some relay designs reach for connection-level authentication to make identity-based policies workable, this is NIP-42. Wotrlay doesn't use that dependency.

On Nostr, events are already signed. The pubkey in the event is the identity that matters for publishing policy. By enforcing rules per event, Wotrlay stays compatible with typical clients while still making the pubkey the unit of accountability.

This is also why Wotrlay doesn't require NIP-42 as a prerequisite for publishing. NIP-42 can be useful in many contexts, but it's not strictly necessary for this specific model: the relay can account for publishing based on the pubkey that signed the event.

Relatr over ContextVM

Wotrlay's only external dependency is the rank provider. ContextVM makes that dependency "Nostr-shaped": you address the service by pubkey, send a request event, and listen for a response event.

This has practical consequences:

If you don't like the default ranking model, you can run your own provider. If a community wants a different notion of rank, they can choose a different provider. If you want to experiment, you can swap the oracle without redesigning the relay.

This also highlights something subtle: DVM calls are fundamentally the same interaction pattern as calling a ContextVM server. Different framing, same Nostr-native mechanism. We'll explore this more in the next article.

What to expect

Wotrlay is not "the solution to moderation." It's a working demonstration of a proof of concept.

Expect a policy that is legible (rank maps to permissions), resistant to cheap sybil throughput (new identities don't get volume), and composable (the ranking oracle can be self-hosted and swapped).

Don't expect a universal rank metric, or a replacement for human judgement where real abuse is involved.

The point is to widen the design space: moderation can be expressed as explicit, programmable constraints—strict where they must be, permissive where they can be, and always replaceable. We would love to hear your feedback, thoughs and ideas.



Want to try it out? We're running a public instance at wss://wotr.relatr.xyz that anyone can connect to and test. It uses the default Relatr instance for ranking.

Want to explore the implementation or run your own instance? Check out the repository at ContextVM/wotrlay for the complete source code, configuration details, and setup instructions.



