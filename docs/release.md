# Creating a New Release

Our release process is fully automated through GitHub Actions, simplifying the way you create new versions of our project. To initiate a new release, follow these straightforward steps:

1. **Prepare the Changelog**: Draft a new Markdown file detailing the changes in this release. Name this file using the pattern `FeOS-$VERSION.md` and place it within the `docs/changelog/` directory. Make sure to replace `$VERSION` with the actual version number of the upcoming release.

2. **Tag the Release**: Once your changelog is in place, tag the corresponding git commit with the release version. Use the format `$VERSION` for the tag, ensuring there is no leading `v`. This tag is crucial as it triggers the automated build and deployment process through GitHub Actions.

By following these simple steps, our GitHub Actions workflow takes care of the rest, automatically building and releasing the new version based on the information you've provided.
