-- ============================================================================
-- Oumma engine — full live schema snapshot (reference only).
--
-- Generated from the running dev database:
--   docker exec <postgres> pg_dump -U oumma -d oumma \
--     --schema-only --no-owner --no-privileges --no-comments
--
-- DO NOT apply this file to a database. The engine auto-creates every table
-- it needs at startup (CREATE TABLE IF NOT EXISTS in internal/repository/*).
-- This snapshot documents the exact structure in production shape, including
-- tables and indexes added by the auth-backend port (oper_*, dms_*, usage_*,
-- email_activations, process_checkpoints, task_*).
-- ============================================================================
--
-- PostgreSQL database dump
--

\restrict cf5HtU7wScfizZLZGWPTtoaVtMvwJaljdCAAtjFAmmkdBuGew6lfIifAZQrlOlk

-- Dumped from database version 16.15
-- Dumped by pg_dump version 16.15

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: design_ai_reviews; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.design_ai_reviews (
    id text NOT NULL,
    process_id text NOT NULL,
    design_version integer DEFAULT 0 NOT NULL,
    prompt text NOT NULL,
    answer text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: dms_document_tags; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.dms_document_tags (
    document_id bigint NOT NULL,
    tag_id bigint NOT NULL
);


--
-- Name: dms_document_versions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.dms_document_versions (
    id bigint NOT NULL,
    document_id bigint NOT NULL,
    version_number integer NOT NULL,
    upload_date timestamp with time zone NOT NULL,
    size bigint NOT NULL,
    type text NOT NULL,
    file_storage_id text NOT NULL
);


--
-- Name: dms_document_versions_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.dms_document_versions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: dms_document_versions_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.dms_document_versions_id_seq OWNED BY public.dms_document_versions.id;


--
-- Name: dms_documents; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.dms_documents (
    id bigint NOT NULL,
    name text NOT NULL,
    type text NOT NULL,
    size bigint NOT NULL,
    upload_date timestamp with time zone NOT NULL,
    status text NOT NULL,
    user_id text DEFAULT ''::text NOT NULL,
    organization_id text,
    process_id text,
    process_instance_id text,
    description text,
    file_path text NOT NULL
);


--
-- Name: dms_documents_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.dms_documents_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: dms_documents_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.dms_documents_id_seq OWNED BY public.dms_documents.id;


--
-- Name: dms_tags; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.dms_tags (
    id bigint NOT NULL,
    name text NOT NULL,
    color text NOT NULL,
    process_id text NOT NULL
);


--
-- Name: dms_tags_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.dms_tags_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: dms_tags_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.dms_tags_id_seq OWNED BY public.dms_tags.id;


--
-- Name: email_activations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.email_activations (
    token text NOT NULL,
    user_id text NOT NULL,
    username text NOT NULL,
    email text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    used_at timestamp with time zone
);


--
-- Name: execution_states; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.execution_states (
    process_id text NOT NULL,
    data jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: llm_logs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.llm_logs (
    id text NOT NULL,
    process_id text NOT NULL,
    task_id text NOT NULL,
    operation text NOT NULL,
    model text NOT NULL,
    system_prompt text NOT NULL,
    user_prompt text NOT NULL,
    response text NOT NULL,
    error text DEFAULT ''::text NOT NULL,
    duration_ms bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    prompt_tokens bigint DEFAULT 0 NOT NULL,
    completion_tokens bigint DEFAULT 0 NOT NULL,
    username text DEFAULT ''::text NOT NULL,
    department text DEFAULT ''::text NOT NULL,
    definition_id text DEFAULT ''::text NOT NULL
);


--
-- Name: oper_form_field_options; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.oper_form_field_options (
    step_field_id bigint NOT NULL,
    ordinal integer NOT NULL,
    value text NOT NULL
);


--
-- Name: oper_form_field_table_field_names; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.oper_form_field_table_field_names (
    step_field_id bigint NOT NULL,
    ordinal integer NOT NULL,
    value text NOT NULL
);


--
-- Name: oper_form_fields; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.oper_form_fields (
    id bigint NOT NULL,
    steps_id bigint NOT NULL,
    field_row integer DEFAULT 0 NOT NULL,
    field_tab text,
    field_type text,
    field_label text,
    field_name text,
    field_value text,
    field_required boolean DEFAULT false NOT NULL,
    field_unique boolean DEFAULT false NOT NULL,
    field_min_length integer,
    field_header_display boolean,
    field_edit boolean DEFAULT true NOT NULL,
    field_template text,
    field_pattern text,
    field_source text,
    field_source_type text,
    field_table_name text,
    user_can_add boolean DEFAULT false NOT NULL,
    field_multple_choice boolean DEFAULT false NOT NULL,
    field_db_query text,
    field_database_user text,
    field_database_password text,
    api_key text
);


--
-- Name: oper_form_fields_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.oper_form_fields_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: oper_form_fields_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.oper_form_fields_id_seq OWNED BY public.oper_form_fields.id;


--
-- Name: oper_process_form_deleted; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.oper_process_form_deleted (
    id bigint NOT NULL,
    process_id text,
    form_id text,
    step_id text,
    version text,
    process_instance_id text,
    step_name text,
    step_description text,
    launchable boolean DEFAULT false NOT NULL,
    form_input text,
    form_fields text,
    deleted boolean DEFAULT false NOT NULL,
    deleted_reason text,
    deleted_at bigint DEFAULT 0 NOT NULL,
    deleted_by text
);


--
-- Name: oper_process_form_deleted_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.oper_process_form_deleted_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: oper_process_form_deleted_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.oper_process_form_deleted_id_seq OWNED BY public.oper_process_form_deleted.id;


--
-- Name: oper_process_forms; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.oper_process_forms (
    id bigint NOT NULL,
    process_id text NOT NULL,
    created_at bigint
);


--
-- Name: oper_process_forms_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.oper_process_forms_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: oper_process_forms_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.oper_process_forms_id_seq OWNED BY public.oper_process_forms.id;


--
-- Name: oper_steps; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.oper_steps (
    id bigint NOT NULL,
    process_form_id bigint NOT NULL,
    form_id text,
    step_id text,
    version text,
    process_instance_id text,
    step_name text,
    step_description text,
    launchable boolean DEFAULT false NOT NULL,
    form_input text,
    created_at bigint
);


--
-- Name: oper_steps_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.oper_steps_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: oper_steps_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.oper_steps_id_seq OWNED BY public.oper_steps.id;


--
-- Name: process_checkpoints; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.process_checkpoints (
    process_id text NOT NULL,
    task_id text NOT NULL,
    phase text NOT NULL,
    prepared_input jsonb,
    task_output jsonb,
    next_task_id text DEFAULT ''::text NOT NULL,
    override text DEFAULT ''::text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: process_definitions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.process_definitions (
    id text NOT NULL,
    data jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: process_designs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.process_designs (
    process_id text NOT NULL,
    version integer NOT NULL,
    design jsonb NOT NULL,
    status text DEFAULT 'saved'::text NOT NULL,
    created_by text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    semver text DEFAULT ''::text NOT NULL
);


--
-- Name: process_track_steps; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.process_track_steps (
    id text NOT NULL,
    process_id text NOT NULL,
    step_order integer NOT NULL,
    from_task_id text NOT NULL,
    to_task_id text DEFAULT ''::text NOT NULL,
    task_type text DEFAULT ''::text NOT NULL,
    status text DEFAULT 'ROUTED'::text NOT NULL,
    reasoning text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: task_attachments; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.task_attachments (
    id text NOT NULL,
    process_id text NOT NULL,
    task_id text NOT NULL,
    task_instance_id text NOT NULL,
    name text NOT NULL,
    uri text NOT NULL,
    group_name text DEFAULT ''::text NOT NULL,
    created_by text DEFAULT ''::text NOT NULL,
    updated_by text DEFAULT ''::text NOT NULL,
    deleted boolean DEFAULT false NOT NULL,
    deleted_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: task_comments; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.task_comments (
    id text NOT NULL,
    process_id text NOT NULL,
    task_id text NOT NULL,
    task_instance_id text NOT NULL,
    content text NOT NULL,
    group_name text DEFAULT ''::text NOT NULL,
    created_by text DEFAULT ''::text NOT NULL,
    updated_by text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: task_histories; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.task_histories (
    id text NOT NULL,
    process_id text NOT NULL,
    chronological_order integer NOT NULL,
    task_id text NOT NULL,
    task_instance_id text DEFAULT ''::text NOT NULL,
    task_type text DEFAULT ''::text NOT NULL,
    owner_id text DEFAULT ''::text NOT NULL,
    groups jsonb,
    prepared_input jsonb,
    task_output jsonb,
    state text DEFAULT 'running'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone,
    size_bytes integer,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: usage_meta; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_meta (
    key text NOT NULL,
    value text NOT NULL
);


--
-- Name: usage_monthly; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_monthly (
    month text NOT NULL,
    username text DEFAULT ''::text NOT NULL,
    department text DEFAULT ''::text NOT NULL,
    process_id text DEFAULT ''::text NOT NULL,
    calls bigint DEFAULT 0 NOT NULL,
    prompt_tokens bigint DEFAULT 0 NOT NULL,
    completion_tokens bigint DEFAULT 0 NOT NULL,
    cost_usd numeric(14,6) DEFAULT 0 NOT NULL
);


--
-- Name: usage_running; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_running (
    username text DEFAULT ''::text NOT NULL,
    department text DEFAULT ''::text NOT NULL,
    process_id text DEFAULT ''::text NOT NULL,
    calls bigint DEFAULT 0 NOT NULL,
    prompt_tokens bigint DEFAULT 0 NOT NULL,
    completion_tokens bigint DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: dms_document_versions id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.dms_document_versions ALTER COLUMN id SET DEFAULT nextval('public.dms_document_versions_id_seq'::regclass);


--
-- Name: dms_documents id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.dms_documents ALTER COLUMN id SET DEFAULT nextval('public.dms_documents_id_seq'::regclass);


--
-- Name: dms_tags id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.dms_tags ALTER COLUMN id SET DEFAULT nextval('public.dms_tags_id_seq'::regclass);


--
-- Name: oper_form_fields id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.oper_form_fields ALTER COLUMN id SET DEFAULT nextval('public.oper_form_fields_id_seq'::regclass);


--
-- Name: oper_process_form_deleted id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.oper_process_form_deleted ALTER COLUMN id SET DEFAULT nextval('public.oper_process_form_deleted_id_seq'::regclass);


--
-- Name: oper_process_forms id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.oper_process_forms ALTER COLUMN id SET DEFAULT nextval('public.oper_process_forms_id_seq'::regclass);


--
-- Name: oper_steps id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.oper_steps ALTER COLUMN id SET DEFAULT nextval('public.oper_steps_id_seq'::regclass);


--
-- Name: design_ai_reviews design_ai_reviews_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.design_ai_reviews
    ADD CONSTRAINT design_ai_reviews_pkey PRIMARY KEY (id);


--
-- Name: dms_document_tags dms_document_tags_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.dms_document_tags
    ADD CONSTRAINT dms_document_tags_pkey PRIMARY KEY (document_id, tag_id);


--
-- Name: dms_document_versions dms_document_versions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.dms_document_versions
    ADD CONSTRAINT dms_document_versions_pkey PRIMARY KEY (id);


--
-- Name: dms_documents dms_documents_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.dms_documents
    ADD CONSTRAINT dms_documents_pkey PRIMARY KEY (id);


--
-- Name: dms_tags dms_tags_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.dms_tags
    ADD CONSTRAINT dms_tags_pkey PRIMARY KEY (id);


--
-- Name: email_activations email_activations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.email_activations
    ADD CONSTRAINT email_activations_pkey PRIMARY KEY (token);


--
-- Name: execution_states execution_states_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.execution_states
    ADD CONSTRAINT execution_states_pkey PRIMARY KEY (process_id);


--
-- Name: llm_logs llm_logs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.llm_logs
    ADD CONSTRAINT llm_logs_pkey PRIMARY KEY (id);


--
-- Name: oper_form_field_options oper_form_field_options_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.oper_form_field_options
    ADD CONSTRAINT oper_form_field_options_pkey PRIMARY KEY (step_field_id, ordinal);


--
-- Name: oper_form_field_table_field_names oper_form_field_table_field_names_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.oper_form_field_table_field_names
    ADD CONSTRAINT oper_form_field_table_field_names_pkey PRIMARY KEY (step_field_id, ordinal);


--
-- Name: oper_form_fields oper_form_fields_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.oper_form_fields
    ADD CONSTRAINT oper_form_fields_pkey PRIMARY KEY (id);


--
-- Name: oper_process_form_deleted oper_process_form_deleted_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.oper_process_form_deleted
    ADD CONSTRAINT oper_process_form_deleted_pkey PRIMARY KEY (id);


--
-- Name: oper_process_forms oper_process_forms_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.oper_process_forms
    ADD CONSTRAINT oper_process_forms_pkey PRIMARY KEY (id);


--
-- Name: oper_steps oper_steps_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.oper_steps
    ADD CONSTRAINT oper_steps_pkey PRIMARY KEY (id);


--
-- Name: process_checkpoints process_checkpoints_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.process_checkpoints
    ADD CONSTRAINT process_checkpoints_pkey PRIMARY KEY (process_id);


--
-- Name: process_definitions process_definitions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.process_definitions
    ADD CONSTRAINT process_definitions_pkey PRIMARY KEY (id);


--
-- Name: process_designs process_designs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.process_designs
    ADD CONSTRAINT process_designs_pkey PRIMARY KEY (process_id, version);


--
-- Name: process_track_steps process_track_steps_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.process_track_steps
    ADD CONSTRAINT process_track_steps_pkey PRIMARY KEY (id);


--
-- Name: task_attachments task_attachments_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.task_attachments
    ADD CONSTRAINT task_attachments_pkey PRIMARY KEY (id);


--
-- Name: task_comments task_comments_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.task_comments
    ADD CONSTRAINT task_comments_pkey PRIMARY KEY (id);


--
-- Name: task_histories task_histories_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.task_histories
    ADD CONSTRAINT task_histories_pkey PRIMARY KEY (id);


--
-- Name: usage_meta usage_meta_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_meta
    ADD CONSTRAINT usage_meta_pkey PRIMARY KEY (key);


--
-- Name: usage_monthly usage_monthly_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_monthly
    ADD CONSTRAINT usage_monthly_pkey PRIMARY KEY (month, username, department, process_id);


--
-- Name: usage_running usage_running_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_running
    ADD CONSTRAINT usage_running_pkey PRIMARY KEY (username, department, process_id);


--
-- Name: idx_design_ai_reviews_process; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_design_ai_reviews_process ON public.design_ai_reviews USING btree (process_id, created_at DESC);


--
-- Name: idx_llm_logs_process; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_llm_logs_process ON public.llm_logs USING btree (process_id, created_at);


--
-- Name: idx_states_definition_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_states_definition_status ON public.execution_states USING btree (((data ->> 'definition_id'::text)), ((data ->> 'status'::text)));


--
-- Name: idx_task_attachments_instance; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_task_attachments_instance ON public.task_attachments USING btree (process_id, task_instance_id, deleted);


--
-- Name: idx_task_comments_instance; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_task_comments_instance ON public.task_comments USING btree (process_id, task_instance_id, created_at);


--
-- Name: idx_task_histories_process_order; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_task_histories_process_order ON public.task_histories USING btree (process_id, chronological_order);


--
-- Name: idx_track_steps_process; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_track_steps_process ON public.process_track_steps USING btree (process_id, step_order);


--
-- Name: dms_document_tags dms_document_tags_document_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.dms_document_tags
    ADD CONSTRAINT dms_document_tags_document_id_fkey FOREIGN KEY (document_id) REFERENCES public.dms_documents(id);


--
-- Name: dms_document_tags dms_document_tags_tag_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.dms_document_tags
    ADD CONSTRAINT dms_document_tags_tag_id_fkey FOREIGN KEY (tag_id) REFERENCES public.dms_tags(id);


--
-- Name: dms_document_versions dms_document_versions_document_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.dms_document_versions
    ADD CONSTRAINT dms_document_versions_document_id_fkey FOREIGN KEY (document_id) REFERENCES public.dms_documents(id);


--
-- Name: oper_form_field_options oper_form_field_options_step_field_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.oper_form_field_options
    ADD CONSTRAINT oper_form_field_options_step_field_id_fkey FOREIGN KEY (step_field_id) REFERENCES public.oper_form_fields(id);


--
-- Name: oper_form_field_table_field_names oper_form_field_table_field_names_step_field_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.oper_form_field_table_field_names
    ADD CONSTRAINT oper_form_field_table_field_names_step_field_id_fkey FOREIGN KEY (step_field_id) REFERENCES public.oper_form_fields(id);


--
-- Name: oper_form_fields oper_form_fields_steps_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.oper_form_fields
    ADD CONSTRAINT oper_form_fields_steps_id_fkey FOREIGN KEY (steps_id) REFERENCES public.oper_steps(id);


--
-- Name: oper_steps oper_steps_process_form_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.oper_steps
    ADD CONSTRAINT oper_steps_process_form_id_fkey FOREIGN KEY (process_form_id) REFERENCES public.oper_process_forms(id);


--
-- PostgreSQL database dump complete
--

\unrestrict cf5HtU7wScfizZLZGWPTtoaVtMvwJaljdCAAtjFAmmkdBuGew6lfIifAZQrlOlk

