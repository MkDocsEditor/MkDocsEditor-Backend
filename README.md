# MkDocsEditor-Backend
Server backend for MkDocsEditor clients.

## How to use

### Configuration

Create a `mkdocsrest.yaml` file similar to the [mkdocsrest_example.yaml](mkdocsrest_example.yaml).

### Docker

Run the service using Docker and mount the configuration file and the wiki folder:

```bash
docker run -d \
    -p 7413:7413 \
    -v ~/mkdocsrest.yaml:/app/mkdocsrest.yaml \
    -v ~/mywiki:/data \
    ghcr.io/mkdocseditor/mkdocseditor-backend:latest
```

## Clients

- [MkDocsEditor-Android](https://github.com/MkDocsEditor/MkDocsEditor-Android)
- [MkDocsEditor-Web](https://github.com/MkDocsEditor/MkDocsEditor-Web)

# Contributing

GitHub is for social coding: if you want to write code, I encourage
contributions through pull requests from forks of this repository.
Create GitHub tickets for bugs and new features and comment on the ones
that you are interested in.

# License

AGPLv3+