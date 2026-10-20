# Subyard presentation

English, 12 slides, 16:9. Designed for a 10–12 minute introduction.

- [Open the browser deck](index.html).
- [Speaker notes and source attribution](notes.html).

Open `index.html` directly in a modern browser. Everything needed to present is local; there are no remote scripts, fonts, analytics, build steps, or runtime dependencies. Keep the directory together when sharing it.

Use **← / →**, **Page Up / Page Down**, or **Space** to move; **Home / End** jump to the first/last slide. **F** toggles fullscreen, **N** opens the current slide's speaker notes. Buttons provide the same controls. Slide numbers are stored in the URL fragment for direct links.

Edit the slide content in `index.html`, layout in `styles.css`, and speaker notes in `notes.html`. Browser printing includes all 12 slides regardless of the currently selected slide. Generated exports are not kept in the repository.

## Web hosting

This directory is a static website and can be served directly by GitHub Pages or any static host. GitHub's repository file viewer displays HTML source; a Pages deployment provides the browser presentation.

For [branch-based GitHub Pages](https://docs.github.com/en/pages/getting-started-with-github-pages/configuring-a-publishing-source-for-your-github-pages-site), after these files reach the chosen publishing branch, select **Settings → Pages → Deploy from a branch**, that branch, and **/docs**. The deck will be at `/presentations/subyard/` under the site's base URL. With the default project domain for this repository, that would be `https://subyard.github.io/Subyard/presentations/subyard/`. This publishes the whole public `docs` directory; it does not require a presentation build step.

Alternatively, a GitHub Actions Pages workflow can upload only this directory and serve the deck at the site's root. Pages must be configured separately; storing these files in Git alone does not publish a site.

## Scope and attribution

Product facts were checked against public commit `4886b6140d0182b0442b2851cadcce79ca5e63e7` on October 20, 2026. The source links in the notes are pinned to that revision. This is a presentation artifact, not a product change.

The opening image is an unmodified screenshot attributed to John Lindquist's May 24, 2025 X post, preserved by [DevMeme](https://devme.me/meme/ai-assistant-nearly-deletes-users-home-directory). Its author and date are visible in the image; the original X status permalink was not recovered, so authenticity was not independently verified. It describes a near miss. The screenshot is third-party material; no license to it is asserted by this deck.

The 32 GiB / approximately four sessions slide is an illustrative budget, not a measured result. It assumes four one-VM staging allocations, a suitably configured pool, 2 GiB per agent workload, and 8 GiB reserved for host/services/overhead. Actual capacity depends on the workload. The default pool has two slots.

Private development, client yards, and a Hermes assistant yard are examples, not built-in product modes. Profiles and agent integrations have separate selections. Existing scoped cache reuse and the broader disk cleanup work in progress are distinguished explicitly.

