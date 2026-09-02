# API Reference

The full API is documented as an OpenAPI 3.0 spec: [docs/openapi.yaml](openapi.yaml).

**To browse it interactively**, paste the file's contents into [editor.swagger.io](https://editor.swagger.io), or run a local Swagger UI:

```bash
docker run -p 8081:8080 -e SWAGGER_JSON=/spec/openapi.yaml -v $(pwd)/docs:/spec swaggerapi/swagger-ui
```

Then open [http://localhost:8081](http://localhost:8081).

All endpoints require `Authorization: Bearer <tfa-token>` except `/healthz` and `/metrics`.
