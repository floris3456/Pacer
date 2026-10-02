#include <assert.h>
#include <dlfcn.h>
#include <stdint.h>
#include <stdlib.h>
#include <stdio.h>
#include <string.h>

typedef struct { void *ptr; size_t len; } buffer;
typedef struct { uint32_t version; void *context; void *call; void *free; } host_api;
typedef struct { uint32_t version; int (*call)(const char*,const uint8_t*,size_t,buffer*); void (*free)(void*,size_t); void (*shutdown)(void); } plugin_api;
int main(int argc,char **argv) {
  assert(argc==4);
  void *lib=dlopen(argv[1],RTLD_NOW|RTLD_LOCAL);
  if (!lib) { fprintf(stderr,"%s\n",dlerror()); return 1; }
  int (*init)(const host_api*,plugin_api*)=dlsym(lib,"cliproxy_plugin_init");
  assert(init);
  host_api host={.version=1}; plugin_api plugin={0};
  assert(init(&host,&plugin)==0 && plugin.version==1);
  const char *request="{}";
  buffer response={0};
  assert(plugin.call("plugin.register",(const uint8_t*)request,strlen(request),&response)==0);
  assert(response.ptr && response.len);
  char *json=calloc(response.len+1,1);memcpy(json,response.ptr,response.len);
  assert(strstr(json,"quota_plugin_error") && strstr(json,"state_path"));
  plugin.free(response.ptr,response.len);free(json);
  FILE *config=fopen(argv[2],"rb"); assert(config);
  char payload[8192]={0}; size_t size=fread(payload,1,sizeof(payload)-1,config);
  assert(size>0 && feof(config)); fclose(config);
  response=(buffer){0};
  assert(plugin.call("plugin.register",(const uint8_t*)payload,size,&response)==0);
  assert(response.ptr && response.len);
  json=calloc(response.len+1,1); memcpy(json,response.ptr,response.len);
  assert(strstr(json,"\"ok\":true") && strstr(json,"\"schema_version\":6"));
  char expected[128];
  assert(snprintf(expected,sizeof(expected),"\"Version\":\"%s\"",argv[3])<(int)sizeof(expected));
  assert(strstr(json,"cliproxyapi-quota-handoff") && strstr(json,expected));
  plugin.free(response.ptr,response.len);free(json);plugin.shutdown();
  puts("Linux C ABI load, failure envelope, dummy registration, buffer ownership and shutdown: passed");
  return 0;
}
