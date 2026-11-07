/* Matched synthetic UI-only GTK4 control, not the production Veranda client. */
#include <gtk/gtk.h>
#include <stdio.h>
#include <string.h>

static GtkWidget *projects, *details, *snapshot, *filter;
static int selected_yard, refreshes;

static void select_project(GtkListBox *box, GtkListBoxRow *row, gpointer unused) {
    (void)box; (void)unused;
    if (!row) return;
    int p = GPOINTER_TO_INT(g_object_get_data(G_OBJECT(row), "project"));
    char text[256];
    snprintf(text, sizeof text, "Project details\n\nproject-%03d\nyard-%02d\nNOT_CREATED\n\nSynthetic data in memory.", p + 1, p / 10 + 1);
    gtk_label_set_text(GTK_LABEL(details), text);
    printf("project %d\n", p + 1); fflush(stdout);
}

static gboolean visible(GtkListBoxRow *row, gpointer unused) {
    (void)unused;
    int p = GPOINTER_TO_INT(g_object_get_data(G_OBJECT(row), "project"));
    char name[32]; snprintf(name, sizeof name, "project-%03d", p + 1);
    return (!selected_yard || p / 10 + 1 == selected_yard) && strstr(name, gtk_editable_get_text(GTK_EDITABLE(filter)));
}

static void select_yard(GtkButton *button, gpointer value) {
    (void)button;
    selected_yard = GPOINTER_TO_INT(value);
    gtk_list_box_invalidate_filter(GTK_LIST_BOX(projects));
    gtk_list_box_select_row(GTK_LIST_BOX(projects), gtk_list_box_get_row_at_index(GTK_LIST_BOX(projects), selected_yard ? (selected_yard - 1) * 10 : 0));
    printf("yard %d\n", selected_yard); fflush(stdout);
}

static void filter_changed(GtkEditable *entry, gpointer unused) {
    (void)unused;
    gtk_list_box_invalidate_filter(GTK_LIST_BOX(projects));
    printf("filter %s\n", gtk_editable_get_text(entry)); fflush(stdout);
}

static void refresh(GtkButton *button, gpointer unused) {
    (void)button; (void)unused;
    char text[80]; snprintf(text, sizeof text, "Snapshot %d · local fixture", ++refreshes);
    gtk_label_set_text(GTK_LABEL(snapshot), text);
    printf("refresh %d\n", refreshes); fflush(stdout);
}

static void activate(GtkApplication *app, gpointer unused) {
    (void)unused;
    GtkWidget *window = gtk_application_window_new(app);
    gtk_window_set_title(GTK_WINDOW(window), "Veranda GTK comparison");
    gtk_window_set_default_size(GTK_WINDOW(window), 1120, 768);
    GtkWidget *root = gtk_box_new(GTK_ORIENTATION_VERTICAL, 12);
    gtk_widget_set_margin_start(root, 12); gtk_widget_set_margin_end(root, 12);
    gtk_widget_set_margin_top(root, 12); gtk_widget_set_margin_bottom(root, 12);
    gtk_window_set_child(GTK_WINDOW(window), root);
    gtk_box_append(GTK_BOX(root), gtk_label_new("Veranda / GTK4 — Synthetic comparison · 10 yards · 100 projects · no backend"));
    GtkWidget *toolbar = gtk_box_new(GTK_ORIENTATION_HORIZONTAL, 12);
    GtkWidget *button = gtk_button_new_with_label("Refresh");
    g_signal_connect(button, "clicked", G_CALLBACK(refresh), NULL);
    gtk_box_append(GTK_BOX(toolbar), button);
    snapshot = gtk_label_new("Snapshot 0 · local fixture"); gtk_box_append(GTK_BOX(toolbar), snapshot);
    gtk_box_append(GTK_BOX(toolbar), gtk_label_new("Filter:"));
    filter = gtk_entry_new(); gtk_widget_set_size_request(filter, 240, -1);
    g_signal_connect(filter, "changed", G_CALLBACK(filter_changed), NULL);
    gtk_box_append(GTK_BOX(toolbar), filter); gtk_box_append(GTK_BOX(root), toolbar);
    GtkWidget *body = gtk_box_new(GTK_ORIENTATION_HORIZONTAL, 12); gtk_widget_set_vexpand(body, TRUE);
    gtk_box_append(GTK_BOX(root), body);
    GtkWidget *yards = gtk_box_new(GTK_ORIENTATION_VERTICAL, 4); gtk_widget_set_size_request(yards, 200, -1);
    gtk_box_append(GTK_BOX(yards), gtk_label_new("Yards"));
    for (int i = 1; i <= 10; ++i) {
        char text[64]; snprintf(text, sizeof text, "yard-%02d     10 projects", i);
        button = gtk_button_new_with_label(text); gtk_widget_set_size_request(button, -1, 38);
        g_signal_connect(button, "clicked", G_CALLBACK(select_yard), GINT_TO_POINTER(i));
        gtk_box_append(GTK_BOX(yards), button);
    }
    button = gtk_button_new_with_label("All projects");
    g_signal_connect(button, "clicked", G_CALLBACK(select_yard), NULL); gtk_box_append(GTK_BOX(yards), button);
    gtk_box_append(GTK_BOX(body), yards);
    projects = gtk_list_box_new(); gtk_list_box_set_filter_func(GTK_LIST_BOX(projects), visible, NULL, NULL);
    for (int i = 0; i < 100; ++i) {
        char text[80]; snprintf(text, sizeof text, "project-%03d                 NOT_CREATED", i + 1);
        GtkWidget *row = gtk_list_box_row_new(), *label = gtk_label_new(text);
        gtk_widget_set_size_request(row, -1, 38); gtk_list_box_row_set_child(GTK_LIST_BOX_ROW(row), label);
        g_object_set_data(G_OBJECT(row), "project", GINT_TO_POINTER(i)); gtk_list_box_append(GTK_LIST_BOX(projects), row);
    }
    GtkWidget *scroll = gtk_scrolled_window_new(); gtk_widget_set_hexpand(scroll, TRUE);
    gtk_scrolled_window_set_child(GTK_SCROLLED_WINDOW(scroll), projects); gtk_box_append(GTK_BOX(body), scroll);
    details = gtk_label_new(""); gtk_label_set_xalign(GTK_LABEL(details), 0); gtk_label_set_yalign(GTK_LABEL(details), 0);
    gtk_widget_set_size_request(details, 250, -1); gtk_box_append(GTK_BOX(body), details);
    g_signal_connect(projects, "row-selected", G_CALLBACK(select_project), NULL);
    gtk_list_box_select_row(GTK_LIST_BOX(projects), gtk_list_box_get_row_at_index(GTK_LIST_BOX(projects), 0));
    gtk_window_present(GTK_WINDOW(window));
}

int main(int argc, char **argv) {
    GtkApplication *app = gtk_application_new("org.subyard.Comparison", G_APPLICATION_DEFAULT_FLAGS);
    g_signal_connect(app, "activate", G_CALLBACK(activate), NULL);
    int status = g_application_run(G_APPLICATION(app), argc, argv);
    g_object_unref(app); return status;
}
