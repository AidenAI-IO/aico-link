import {byId, registerRuntime, runtimeFunction, runtime} from './state.js';
const hideOtaLogPanel=runtimeFunction('hideOtaLogPanel');
let bannerTimer=null;
let bannerGeneration=0;
function updateActionCardVisibility(){const card=byId('actionCard');if(!card)return;const banner=byId('actionBanner');const application=byId('configApplication');const detailText=byId('actionDetailsText');const ota=byId('otaLogPanel');const hasBanner=!!(banner&&banner.style.display==='block'&&banner.textContent);const hasApplication=!!(application&&application.style.display==='block');const hasDetails=!!(detailText&&detailText.style.display==='block'&&detailText.textContent)||!!(ota&&ota.style.display==='block');card.style.display=(hasBanner||hasApplication||hasDetails)?'block':'none';}
function setBanner(message,isError){const el=byId('actionBanner');if(!el)return;bannerGeneration++;const generation=bannerGeneration;if(bannerTimer){clearTimeout(bannerTimer);bannerTimer=null;}el.textContent=message||'';el.className='banner'+(isError?' error':'');el.style.display=message?'block':'none';updateActionCardVisibility();if(message&&!isError){bannerTimer=setTimeout(function(){if(generation!==bannerGeneration)return;el.style.display='none';bannerTimer=null;updateActionCardVisibility();},4000);}}
    function updateActionDetailsVisibility(){const wrap=byId('actionDetails');const body=byId('actionDetailsText');const ota=byId('otaLogPanel');const hasText=!!(body&&body.style.display==='block'&&body.textContent);const hasOta=!!(ota&&ota.style.display==='block');wrap.style.display=(hasText||hasOta)?'block':'none';updateActionCardVisibility();}
    function setDetails(text,options){const body=byId('actionDetailsText');if(!(options&&options.keepOtaLog)){hideOtaLogPanel();}if(text){body.style.display='block';body.textContent=text;}else{body.style.display='none';body.textContent='';}updateActionDetailsVisibility();}
function isConfigWrite(url, options) {
  return ['/api/config', '/api/config/locale', '/api/config/backup'].includes(url)
    && !!options?.method && options.method !== 'GET';
}
    function redirectToLogin(res,body){if(res.status!==401||body.error!=='login_required'||typeof window==='undefined'||!window.location)return;const next=window.location.pathname+window.location.search+window.location.hash;window.location.assign('/login?next='+encodeURIComponent(next));}
    async function performRequest(url,options){const res=await fetch(url,options);const text=await res.text();let body={};try{body=text?(JSON.parse(text)??{}):{}}catch(err){body={ok:false,error:text||err.message}}redirectToLogin(res,body);if(!res.ok){if(isConfigWrite(url,options)&&body.persisted&&runtime.configApplicationSaved)runtime.configApplicationSaved({...body,state:'failed'});const error=new Error(body.error||('HTTP '+res.status));if(body&&typeof body==='object')Object.keys(body).forEach(function(key){error[key]=body[key];});error.status=res.status;throw error;}if(isConfigWrite(url,options)){if(runtime.configApplicationSaved)runtime.configApplicationSaved(body);}return body;}

let configSaves = Promise.resolve();
function request(url, options) {
  if (isConfigWrite(url, options)) {
    const pending = configSaves.then(() => performRequest(url, options));
    configSaves = pending.catch(() => {});
    return pending;
  }
  return performRequest(url, options);
}

export { request, setBanner, setDetails, updateActionCardVisibility, updateActionDetailsVisibility };
registerRuntime({ request, setBanner, setDetails, updateActionCardVisibility, updateActionDetailsVisibility });
