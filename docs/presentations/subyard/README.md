# Where agents work

English, 11 slides, 16:9. A 10–12 minute talk about requirements for the environment where agents work: isolation, separate trust contexts, shared rules and optional profiles, temporary resources, leases, cache and disk policies, and scoped credentials. Subyard is one concrete implementation example.

The opening problem leads into the container/VM comparison on slide 2, shared capacity on slide 3, and separate workspaces with their own tools, access, and resources on slide 4. The remaining slides build up operational requirements and close with criteria for evaluating an implementation.

- [Open the browser deck](https://subyard.github.io/subyard/presentations/subyard/).
- [Speaker notes and source attribution](https://subyard.github.io/subyard/presentations/subyard/notes.html).

Open `index.html` directly in a modern browser. Everything needed to present is local; there are no remote scripts, fonts, analytics, build steps, or runtime dependencies. Keep the directory together when sharing it.

Use **← / →**, **Page Up / Page Down**, or **Space** to move; **Home / End** jump to the first/last slide. **F** toggles fullscreen, **N** opens the current slide's speaker notes. Buttons provide the same controls. Slide numbers are stored in the URL fragment for direct links.

Edit the slide content in `index.html`, layout in `styles.css`, and speaker notes in `notes.html`. Browser printing includes all 11 slides regardless of the currently selected slide. Generated exports are not kept in the repository.

## Web hosting

This directory is a static website and can be served directly by GitHub Pages or any static host. GitHub's repository file viewer displays HTML source; a Pages deployment provides the browser presentation.

For [branch-based GitHub Pages](https://docs.github.com/en/pages/getting-started-with-github-pages/configuring-a-publishing-source-for-your-github-pages-site), after these files reach the chosen publishing branch, select **Settings → Pages → Deploy from a branch**, that branch, and **/docs**. The deck will be at `/presentations/subyard/` under the site's base URL. For this repository, it is `https://subyard.github.io/subyard/presentations/subyard/`. This publishes the whole public `docs` directory; it does not require a presentation build step.

Alternatively, a GitHub Actions Pages workflow can upload only this directory and serve the deck at the site's root. Pages must be configured separately; storing these files in Git alone does not publish a site.

## Scope and attribution

Subyard implementation examples use public commit `4886b6140d0182b0442b2851cadcce79ca5e63e7` as their baseline. Slide 5's real tool/profile selections were checked against `8a2e38e147153ceab6019899078f5ae3acc2a188`; its notes pin the updated sources. Product defaults illustrate the requirements; they do not define what every implementation must do.

The opening slide links directly to [u/LovesWorkin’s original Reddit post](https://www.reddit.com/r/ClaudeAI/comments/1pgxckk/claude_cli_deleted_my_entire_home_directory_wiped/) in r/ClaudeAI, dated December 7, 2025, and uses its [original terminal attachment](https://i.redd.it/egjqmw80bv5g1.png) without alteration. The author reports home-directory deletion during repository cleanup; the incident and cause are not independently verified. The image is third-party material and is not relicensed by this deck.

The shared 32 GB slide illustrates five container workspaces with overlapping memory limits and heavy resources leased on demand. Its snapshot has 5 GB of agent work, a 6 GB emulator, a 10 GB disposable VM, 3 GB of host/services, and 8 GB free. Limits are ceilings rather than reservations; their sum can exceed physical RAM when workload peaks do not coincide. The numbers illustrate the desired operating model, not measured throughput or Subyard defaults. Admission must still account for available capacity and headroom.

Private development, client work, and a Hermes assistant workspace are illustrative trust contexts. In Subyard, profiles and agent integrations have separate selections. Its existing scoped cache reuse and broader disk cleanup work in progress are distinguished explicitly; the need for cleanup applies even where an implementation is unfinished.

Slide 5 shows real Codex, Orca, Paseo, Codex Desktop, OpenLIT, and GitHub broker capabilities with example selections across dev, OpenClaw, and personal yards. Codex Desktop is a client connection. AI Observer is identified as a retired analytics profile, not a currently available selection. The GitHub broker example is enabled for both dev and OpenClaw.

Slide 10 pairs efficient use of compute across available hosts with capabilities exposed to agents through discoverable skills. Borrowing heavy resources from other Subyards is explicitly in planning. The shipped skill example is `subyard-github`, installed by the selected GitHub profile for supported agents; this does not imply skill coverage for every Subyard function. Its notes pin the implementation sources to `8a2e38e147153ceab6019899078f5ae3acc2a188`.
