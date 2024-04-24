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

### Connect

Use a client to connect to the service.

## Clients

- [MkDocsEditor-Android](https://github.com/MkDocsEditor/MkDocsEditor-Android)
- [MkDocsEditor-Web](https://github.com/MkDocsEditor/MkDocsEditor-Web)

## API

[OpenAPI Documentation](https://editor-next.swagger.io/?url=https://raw.githubusercontent.com/MkDocsEditor/MkDocsEditor-Backend/master/openapi-description.yaml)

### General

| Method | Path           | Description                                 |
|--------|----------------|---------------------------------------------|
| GET    | /alive         | Liveness probe endpoint                     |
| GET    | /mkdocs/config | Retrieve the `mkdocsrest.yaml` configurtion |

### Sections

| Method | Path                 | Description                                           |
|--------|----------------------|-------------------------------------------------------|
| GET    | /section             | Retrieve the whole section tree                       |
| GET    | /section/<sectionId> | Retrieve the section with the given `sectionId`       |
| POST   | /section             | Create a new secton                                   |
| PUT    | /section/<sectionId> | Update an existing section with the given `sectionId` |
| DELETE | /section/<sectionId> | Delete the section with the given `sectionId`         |

### Documents

| Method | Path                           | Description                                                                                                 |
|--------|:-------------------------------|-------------------------------------------------------------------------------------------------------------|
| GET    | /document/<documentId>         | Retrieve the document with the given `documentId`                                                           |
| GET    | /document/<documentId>/ws      | Websocket endpoint for realtime communication regarding updates of the document with the given `documentId` |
| GET    | /document/<documentId>/content | Retrieve the current content of the document with the given `documentId`                                    |
| POST   | /document                      | Create a new document                                                                                       |
| PUT    | /document/<documentId>         | Rename an exsiting document with the given `documentId`                                                     |
| DELETE | /document/<documentId>         | Delete the document with the given `documentId`                                                             |

### Resources

| Method | Path                           | Description                                                              |
|--------|--------------------------------|--------------------------------------------------------------------------|
| GET    | /resource/<resourceId>         | Retrieve the resource with the given `resourceId`                        |
| GET    | /resource/<resourceId>/content | Retrieve the current content of the resource with the given `resourceId` |
| POST   | /resource                      | Upload a new resource                                                    |
| PUT    | /resource/<resourceId>         | Rename an exsiting resource with the given `resourceId`                  |
| DELETE | /resource/<resourceId>         | Delete the resource with the given `resourceId`                          |

# Contributing

GitHub is for social coding: if you want to write code, I encourage
contributions through pull requests from forks of this repository.
Create GitHub tickets for bugs and new features and comment on the ones
that you are interested in.

# License

AGPLv3+
